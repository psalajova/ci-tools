package ocp

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/sirupsen/logrus"

	kyaml "sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools/pkg/clusterinit/clusterinstall"
	"github.com/openshift/ci-tools/pkg/clusterinit/types"
)

type CreateInstallConfigStepOption func(*createInstallConfigStep)

func WithReadFile(fn func(string) ([]byte, error)) CreateInstallConfigStepOption {
	return func(s *createInstallConfigStep) { s.readFile = fn }
}

func WithWriteFile(fn func(string, []byte, os.FileMode) error) CreateInstallConfigStepOption {
	return func(s *createInstallConfigStep) { s.writeFile = fn }
}

type createInstallConfigStep struct {
	log            *logrus.Entry
	clusterInstall *clusterinstall.ClusterInstall
	cmdBuilder     types.CmdBuilder
	cmdRunner      types.CmdRunner
	readFile       func(string) ([]byte, error)
	writeFile      func(string, []byte, os.FileMode) error
}

func (s *createInstallConfigStep) Name() string {
	return "create-ocp-install-config"
}

func (s *createInstallConfigStep) Run(ctx context.Context) error {
	log := s.log.WithField("step", "provision: ocp: install-config")

	cmd := s.cmdBuilder(ctx, "openshift-install", "create", "install-config", "--log-level=debug",
		fmt.Sprintf("--dir=%s", path.Join(s.clusterInstall.InstallBase, "ocp-install-base")))

	log.Info("Creating install-config")
	if err := s.cmdRunner(cmd); err != nil {
		return fmt.Errorf("create install-config: %w", err)
	}

	log.Info("Patching install-config")
	installConfigPath := path.Join(s.clusterInstall.InstallBase, "ocp-install-base", "install-config.yaml")
	if err := s.patchInstallConfig(installConfigPath); err != nil {
		return fmt.Errorf("patch install-config: %w", err)
	}

	return nil
}

func (s *createInstallConfigStep) patchInstallConfig(installConfigPath string) error {
	installConfigBytes, err := s.readFile(installConfigPath)
	if err != nil {
		return fmt.Errorf("read file %s: %w", installConfigPath, err)
	}

	installConfigPatched, err := s.patchControlPlaneAWS(installConfigBytes)
	if err != nil {
		return fmt.Errorf("patch control plane %s: %w", installConfigPath, err)
	}

	if installConfigPatched == nil {
		return nil
	}

	if err := s.writeFile(installConfigPath, installConfigPatched, 0644); err != nil {
		return fmt.Errorf("write file %s: %w", installConfigPath, err)
	}

	return nil
}

func (s *createInstallConfigStep) patchControlPlaneAWS(installConfigBytes []byte) ([]byte, error) {
	installConfig := make(map[string]any)
	if err := kyaml.Unmarshal(installConfigBytes, &installConfig); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	getObj := func(obj any, path string) any {
		for key := range strings.SplitSeq(path, "/") {
			mapObj, ok := obj.(map[string]any)
			if !ok {
				return nil
			}

			if subObj, ok := mapObj[key]; ok {
				obj = subObj
			} else {
				return nil
			}
		}
		return obj
	}

	platformObj := getObj(installConfig, "controlPlane/platform")
	if platformObj == nil {
		s.log.Warn("controlPlane/platform stanza not found")
		return nil, nil
	}

	awsObj := getObj(platformObj, "aws")
	if awsObj == nil {
		s.log.Warn("aws not found, skip patching")
		return nil, nil
	}

	var rootVolume map[string]any
	aws := awsObj.(map[string]any)
	if _, ok := aws["rootVolume"]; !ok {
		rootVolume = make(map[string]any)
		aws["rootVolume"] = rootVolume
	} else {
		rootVolume = aws["rootVolume"].(map[string]any)
	}

	rootVolume["throughput"] = 250

	installConfigPatched, err := kyaml.Marshal(installConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	return installConfigPatched, nil
}

func NewCreateInstallConfigStep(log *logrus.Entry, clusterInstall *clusterinstall.ClusterInstall,
	cmdBuilder types.CmdBuilder, cmdRunner types.CmdRunner, opts ...CreateInstallConfigStepOption) *createInstallConfigStep {
	s := &createInstallConfigStep{
		log:            log,
		clusterInstall: clusterInstall,
		cmdBuilder:     cmdBuilder,
		cmdRunner:      cmdRunner,
		readFile:       os.ReadFile,
		writeFile:      os.WriteFile,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}
