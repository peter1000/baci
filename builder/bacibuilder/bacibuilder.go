package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/sgotti/baci/builder/docker"
	"github.com/sgotti/baci/builder/util"
	"github.com/sgotti/baci/common"

	"github.com/sgotti/baci/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/sgotti/baci/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/sgotti/baci/Godeps/_workspace/src/github.com/sgotti/acibuilder"
)

const (
	authorAnnotation = "author"
)

var (
	excludePaths = []*regexp.Regexp{
		regexp.MustCompile("^dev/.+"),
		regexp.MustCompile("^proc/.+"),
		regexp.MustCompile("^sys/.+"),
		regexp.MustCompile("^baci$"),
		regexp.MustCompile("^baci/.+"),
	}
)

func die(s string, i ...interface{}) {
	s = fmt.Sprintf(s, i...)
	fmt.Fprintln(os.Stderr, strings.TrimSuffix(s, "\n"))
	os.Exit(1)
}

type Builder interface {
	Build() error
	GetBaseImage() (string, error)
	GetExec() []string
	GetUser() string
	GetGroup() string
	GetEnv() map[string]string
	GetWorkDir() string
	GetPorts() ([]types.Port, error)
	GetMountPoints() ([]types.MountPoint, error)
	GetMaintainer() (string, error)
}

func NewExcludeFunc(exclude []*regexp.Regexp) acibuilder.ExcludeFunc {
	return func(path string, info os.FileInfo) (bool, error) {
		for _, excludePath := range excludePaths {
			if excludePath.Match([]byte(path)) {
				return true, nil
			}
		}
		return false, nil
	}
}

func BuildACI(root string, destfile string, configData *common.ConfigData, b Builder) error {
	aciBuilder := acibuilder.NewSimpleACIBuilder(root)
	aciBuilder.SetExcludeFunc(NewExcludeFunc(excludePaths))

	mode := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	fh, err := os.OpenFile(destfile, mode, 0644)
	if err != nil {
		return fmt.Errorf("unable to open target image %s: %v", destfile, err)
	}
	defer func() {
		fh.Close()
	}()

	// TODO(sgotti) as a shell cannot be executed by rocket (as it's runned
	// inside a systemd unit and it will exit), replace it with something
	// else?
	exec := b.GetExec()
	if len(exec) == 0 {
		// Set a default exec
		exec = []string{"/bin/bash"}
	}
	user := b.GetUser()
	if user == "" {
		user = "0"
	}
	group := b.GetGroup()
	if group == "" {
		group = "0"
	}
	env := b.GetEnv()
	workDir := b.GetWorkDir()
	ports, err := b.GetPorts()
	if err != nil {
		return err
	}
	maintainer, err := b.GetMaintainer()
	if err != nil {
		return err
	}
	environment := types.Environment{}
	for k, v := range env {
		environment.Set(k, v)
	}

	annotations := &types.Annotations{}
	// Add an "author" annotation if maintainers isn't empty
	if maintainer != "" {
		annotations.Set(authorAnnotation, maintainer)
	}

	app := &types.App{
		Exec:             exec,
		User:             user,
		Group:            group,
		Environment:      environment,
		WorkingDirectory: workDir,
		Ports:            ports,
	}

	im := schema.ImageManifest{
		ACKind:      types.ACKind("ImageManifest"),
		ACVersion:   schema.AppContainerVersion,
		Name:        configData.AppName,
		Labels:      configData.Labels,
		App:         app,
		Annotations: *annotations,
	}

	err = aciBuilder.Build(im, fh)
	if err != nil {
		return err
	}
	return nil
}

// makedev mimics glib's gnu_dev_makedev
func makedev(major, minor int) int {
	return (minor & 0xff) | (major & 0xfff << 8) | int((uint64(minor & ^0xff) << 12)) | int(uint64(major & ^0xfff)<<32)
}

func main() {
	log.Printf("Starting the baci aci!\n")

	// TODO(sgotti) Hack as /dev/null is needed to run xz (this should be provided by coreos/rocket#540)
	um := syscall.Umask(0)
	os.MkdirAll("/dev", 0755)
	dev := makedev(1, 3)
	mode := uint32(0666) | syscall.S_IFCHR
	if err := syscall.Mknod("/dev/null", mode, dev); err != nil {
		if !os.IsExist(err) {
			die("error: %v", err)
		}
	}
	syscall.Umask(um)

	configDataJson, err := ioutil.ReadFile(filepath.Join(common.BaciDataDir, "configdata"))
	if err != nil {
		die("cannot read the configdata file: %v", err)
	}
	var configData common.ConfigData
	err = json.Unmarshal(configDataJson, &configData)
	if err != nil {
		die("cannot unmarshal configdata: %v", err)
	}

	var root string
	if len(os.Args) > 1 {
		// Useful for local tests outside a container
		root = os.Args[1]
	} else {
		root = "/"
	}
	sourceDir := filepath.Join(root, common.BaciSourceDir)

	baseACIPath := filepath.Join(common.BaciDataDir, "base.aci")
	if _, err := os.Stat(baseACIPath); err == nil {
		log.Printf("Extracting base aci\n")
		r, err := os.Open(baseACIPath)
		if err != nil {
			die("cannot open source file: %v", err)
		}
		tr := tar.NewReader(r)
		err = util.ExtractTarRootFS(tr, root, true)
		if err != nil {
			die("error extracting tar: %v", err)
		}
	}

	// Remove base.aci
	os.Remove(baseACIPath)

	builder, err := docker.NewDockerBuilder(root, sourceDir)
	if err != nil {
		die("error: %v", err)
	}
	err = builder.Build()
	if err != nil {
		die("error: %v", err)
	}

	log.Printf("Building the ACI...")
	err = BuildACI(root, filepath.Join(common.BaciDestDir, configData.OutFile), &configData, builder)
	if err != nil {
		die("error: %v", err)
	}

	log.Printf("Done!\n")
}
