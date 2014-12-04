/*
 * Copyright 2014 VMware, Inc.  All rights reserved.  Licensed under the Apache v2 License.
 */

package vsphere

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	log "github.com/Sirupsen/logrus"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/utils"
	"github.com/docker/machine/drivers"
	"github.com/docker/machine/drivers/vsphere/errors"
	"github.com/docker/machine/ssh"
	"github.com/docker/machine/state"
)

const (
	DATASTORE_DIR      = "boot2docker-iso"
	B2D_ISO_NAME       = "boot2docker.iso"
	DEFAULT_CPU_NUMBER = 2
)

type Driver struct {
	MachineName    string
	SSHPort        int
	CPU            int
	Memory         int
	DiskSize       int
	Boot2DockerURL string
	IP             string
	Username       string
	Password       string
	Network        string
	Datastore      string
	Datacenter     string
	Pool           string
	HostIP         string
	StorePath      string
	ISO            string

	storePath string
}

type CreateFlags struct {
	CPU            *int
	Memory         *int
	DiskSize       *int
	Boot2DockerURL *string
	IP             *string
	Username       *string
	Password       *string
	Network        *string
	Datastore      *string
	Datacenter     *string
	Pool           *string
	HostIP         *string
}

func init() {
	drivers.Register("vsphere", &drivers.RegisteredDriver{
		New:                 NewDriver,
		RegisterCreateFlags: RegisterCreateFlags,
	})
}

func RegisterCreateFlags(cmd *flag.FlagSet) interface{} {
	createFlags := new(CreateFlags)
	createFlags.CPU = cmd.Int([]string{"-vsphere-cpu"}, 2, "vSphere CPU number for docker VM")
	createFlags.Memory = cmd.Int([]string{"-vsphere-memory"}, 2048, "vSphere size of memory for docker VM (in MB)")
	createFlags.DiskSize = cmd.Int([]string{"-vsphere-disk-size"}, 20000, "vSphere size of disk for docker VM (in MB)")
	createFlags.Boot2DockerURL = cmd.String([]string{"-vsphere-boot2docker-url"}, "", "vSphere URL for boot2docker image")
	createFlags.IP = cmd.String([]string{"-vsphere-vcenter"}, "", "vSphere IP/hostname for vCenter")
	createFlags.Username = cmd.String([]string{"-vsphere-username"}, "", "vSphere username")
	createFlags.Password = cmd.String([]string{"-vsphere-password"}, "", "vSphere password")
	createFlags.Network = cmd.String([]string{"-vsphere-network"}, "", "vSphere network where the docker VM will be attached")
	createFlags.Datastore = cmd.String([]string{"-vsphere-datastore"}, "", "vSphere datastore for docker VM")
	createFlags.Datacenter = cmd.String([]string{"-vsphere-datacenter"}, "", "vSphere datacenter for docker VM")
	createFlags.Pool = cmd.String([]string{"-vsphere-pool"}, "", "vSphere resource pool for docker VM")
	createFlags.HostIP = cmd.String([]string{"-vsphere-compute-ip"}, "", "vSphere compute host IP where the docker VM will be instantiated")
	return createFlags
}

func NewDriver(storePath string) (drivers.Driver, error) {
	return &Driver{StorePath: storePath}, nil
}

func (d *Driver) DriverName() string {
	return "vsphere"
}

func (d *Driver) SetConfigFromFlags(flagsInterface interface{}) error {
	flags := flagsInterface.(*CreateFlags)
	d.setMachineNameIfNotSet()
	d.SSHPort = 22
	d.CPU = *flags.CPU
	d.Memory = *flags.Memory
	d.DiskSize = *flags.DiskSize
	d.Boot2DockerURL = *flags.Boot2DockerURL
	d.IP = *flags.IP
	d.Username = *flags.Username
	d.Password = *flags.Password
	d.Network = *flags.Network
	d.Datastore = *flags.Datastore
	d.Datacenter = *flags.Datacenter
	d.Pool = *flags.Pool
	d.HostIP = *flags.HostIP

	d.ISO = path.Join(d.storePath, "boot2docker.iso")

	return nil
}

func (d *Driver) GetURL() (string, error) {
	ip, _ := d.GetIP()
	if ip == "" {
		return "", nil
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetIP() (string, error) {
	status, err := d.GetState()
	if status != state.Running {
		return "", errors.NewInvalidStateError(d.MachineName)
	}
	vcConn := NewVcConn(d)
	rawIp, err := vcConn.VmFetchIp()
	if err != nil {
		return "", err
	}
	ip := strings.Trim(strings.Split(rawIp, "\n")[0], " ")
	return ip, nil
}

func (d *Driver) GetState() (state.State, error) {
	vcConn := NewVcConn(d)
	stdout, err := vcConn.VmInfo()
	if err != nil {
		return state.None, err
	}

	if strings.Contains(stdout, "poweredOn") {
		return state.Running, nil
	} else if strings.Contains(stdout, "poweredOff") {
		return state.Stopped, nil
	}
	return state.None, nil
}

// the current implementation does the following:
// 1. check whether the docker directory contains the boot2docker ISO
// 2. generate an SSH keypair
// 3. create a virtual machine with the boot2docker ISO mounted;
// 4. reconfigure the virtual machine network and disk size;
func (d *Driver) Create() error {
	d.setMachineNameIfNotSet()

	if err := d.checkVsphereConfig(); err != nil {
		return err
	}

	var isoURL string
	if d.Boot2DockerURL != "" {
		isoURL = d.Boot2DockerURL
	} else {
		// HACK: Docker 1.3 boot2docker image with identity auth and vmtoolsd
		isoURL = "https://github.com/cloudnativeapps/boot2docker/releases/download/1.3.1_vmw-identity/boot2docker.iso"
	}
	log.Infof("Downloading boot2docker...")
	if err := downloadISO(d.storePath, "boot2docker.iso", isoURL); err != nil {
		return err
	}

	log.Infof("Generating SSH Keypair...")
	if err := ssh.GenerateSSHKey(d.sshKeyPath()); err != nil {
		return err
	}

	vcConn := NewVcConn(d)
	log.Infof("Uploading Boot2docker ISO ...")
	if err := vcConn.DatastoreMkdir(DATASTORE_DIR); err != nil {
		return err
	}

	if _, err := os.Stat(d.ISO); os.IsNotExist(err) {
		log.Errorf("Unable to find boot2docker ISO at %s", d.ISO)
		return errors.NewIncompleteVsphereConfigError(d.ISO)
	}

	if err := vcConn.DatastoreUpload(d.ISO); err != nil {
		return err
	}

	isoPath := fmt.Sprintf("%s/%s", DATASTORE_DIR, B2D_ISO_NAME)
	if err := vcConn.VmCreate(isoPath); err != nil {
		return err
	}

	log.Infof("Configuring the virtual machine %s... ", d.MachineName)
	if err := vcConn.VmDiskCreate(); err != nil {
		return err
	}

	if err := vcConn.VmAttachNetwork(); err != nil {
		return err
	}

	if err := d.Start(); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Start() error {
	machineState, err := d.GetState()
	if err != nil {
		return err
	}

	switch machineState {
	case state.Running:
		log.Infof("VM %s has already been started", d.MachineName)
		return nil
	case state.Stopped:
		// TODO add transactional or error handling in the following steps
		vcConn := NewVcConn(d)
		err := vcConn.VmPowerOn()
		if err != nil {
			return err
		}
		// this step waits for the vm to start and fetch its ip address;
		// this guarantees that the opem-vmtools has started working...
		_, err = vcConn.VmFetchIp()
		if err != nil {
			return err
		}

		log.Infof("Configuring virtual machine %s... ", d.MachineName)
		err = vcConn.GuestMkdir("docker", "tcuser", "/home/docker/.ssh")
		if err != nil {
			return err
		}

		// configure the ssh key pair and download the pem file
		err = vcConn.GuestUpload("docker", "tcuser", d.publicSSHKeyPath(),
			"/home/docker/.ssh/authorized_keys")
		if err != nil {
			return err
		}

		// Add identity authorization keys
		if err := drivers.AddPublicKeyToAuthorizedHosts(d, "/root/.docker/authorized-keys.d"); err != nil {
			return err
		}

		// Restart Docker
		cmd, err := d.GetSSHCommand("sudo /etc/init.d/docker restart")
		if err != nil {
			return err
		}
		if err := cmd.Run(); err != nil {
			return err
		}

		return nil
	}
	return errors.NewInvalidStateError(d.MachineName)
}

func (d *Driver) Stop() error {
	vcConn := NewVcConn(d)
	err := vcConn.VmPowerOff()
	if err != nil {
		return err
	}
	return err
}

func (d *Driver) Remove() error {
	machineState, err := d.GetState()
	if err != nil {
		return err
	}
	if machineState == state.Running {
		if err = d.Stop(); err != nil {
			return fmt.Errorf("can't stop VM: %s", err)
		}
	}
	vcConn := NewVcConn(d)
	err = vcConn.VmDestroy()
	if err != nil {
		return err
	}
	return nil
}

func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

func (d *Driver) Kill() error {
	return d.Stop()
}

func (d *Driver) Upgrade() error {
	return fmt.Errorf("upgrade is not supported for vsphere driver at this moment")
}

func (d *Driver) GetSSHCommand(args ...string) (*exec.Cmd, error) {
	ip, err := d.GetIP()
	if err != nil {
		return nil, err
	}
	return ssh.GetSSHCommand(ip, d.SSHPort, "docker", d.sshKeyPath(), args...), nil
}

func (d *Driver) setMachineNameIfNotSet() {
	if d.MachineName == "" {
		d.MachineName = generateVMName()
	}
}

func (d *Driver) sshKeyPath() string {
	return filepath.Join(d.StorePath, "id_docker_host_vsphere")
}

func (d *Driver) publicSSHKeyPath() string {
	return d.sshKeyPath() + ".pub"
}

func (d *Driver) checkVsphereConfig() error {
	if d.IP == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere IP")
	}
	if d.Username == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere username")
	}
	if d.Password == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere password")
	}
	if d.Network == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere network")
	}
	if d.Datastore == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere datastore")
	}
	if d.Datacenter == "" {
		return errors.NewIncompleteVsphereConfigError("vSphere datacenter")
	}
	return nil
}

// Download boot2docker ISO image for the given tag and save it at dest.
func downloadISO(dir, file, url string) error {
	rsp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()

	// Download to a temp file first then rename it to avoid partial download.
	f, err := ioutil.TempFile(dir, file+".tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, rsp.Body); err != nil {
		// TODO: display download progress?
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path.Join(dir, file)); err != nil {
		return err
	}
	return nil
}

func generateVMName() string {
	randomID := utils.TruncateID(utils.GenerateRandomID())
	return fmt.Sprintf("docker-host-%s", randomID)
}
