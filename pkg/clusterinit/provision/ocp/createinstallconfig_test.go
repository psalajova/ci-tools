package ocp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/openshift/ci-tools/pkg/clusterinit/clusterinstall"
)

func TestCreateInstallConfigStepRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		ci           *clusterinstall.ClusterInstall
		inputYAML    string
		runCmdErr    error
		readFileErr  error
		writeFileErr error
		wantCmdArgs  []string
		wantPath     string
		wantData     string
		wantErr      string
	}{
		{
			name:        "Creates install-config and patches AWS rootVolume throughput",
			ci:          &clusterinstall.ClusterInstall{InstallBase: "/cluster-base"},
			wantCmdArgs: []string{"create", "install-config", "--log-level=debug", "--dir=/cluster-base/ocp-install-base"},
			wantPath:    "/cluster-base/ocp-install-base/install-config.yaml",
			wantData: `apiVersion: v1
controlPlane:
  platform:
    aws:
      rootVolume:
        size: 150
        throughput: 250
        type: gp2
`,
			inputYAML: `apiVersion: v1
controlPlane:
  platform:
    aws:
      rootVolume:
        size: 150
        type: gp2
`,
		},
		{
			name:        "Creates rootVolume when missing",
			ci:          &clusterinstall.ClusterInstall{InstallBase: "/install"},
			wantCmdArgs: []string{"create", "install-config", "--log-level=debug", "--dir=/install/ocp-install-base"},
			wantPath:    "/install/ocp-install-base/install-config.yaml",
			wantData: `apiVersion: v1
controlPlane:
  platform:
    aws:
      rootVolume:
        throughput: 250
      type: m5.2xlarge
`,
			inputYAML: `apiVersion: v1
controlPlane:
  platform:
    aws:
      type: m5.2xlarge
`,
		},
		{
			name:        "Fails when cannot read install-config file",
			ci:          &clusterinstall.ClusterInstall{InstallBase: "/cluster-base"},
			wantCmdArgs: []string{"create", "install-config", "--log-level=debug", "--dir=/cluster-base/ocp-install-base"},
			readFileErr: errors.New("permission denied"),
			wantErr:     "patch install-config: read file /cluster-base/ocp-install-base/install-config.yaml: permission denied",
		},
		{
			name:         "Fails when cannot write install-config file",
			ci:           &clusterinstall.ClusterInstall{InstallBase: "/cluster-base"},
			wantCmdArgs:  []string{"create", "install-config", "--log-level=debug", "--dir=/cluster-base/ocp-install-base"},
			writeFileErr: errors.New("disk full"),
			inputYAML: `apiVersion: v1
controlPlane:
  platform:
    aws:
      rootVolume:
        size: 150
`,
			wantErr: "patch install-config: write file /cluster-base/ocp-install-base/install-config.yaml: disk full",
		},
		{
			name:        "Fails when command execution fails",
			ci:          &clusterinstall.ClusterInstall{InstallBase: "/cluster-base"},
			wantCmdArgs: []string{"create", "install-config", "--log-level=debug", "--dir=/cluster-base/ocp-install-base"},
			runCmdErr:   errors.New("command failed"),
			wantErr:     "create install-config: command failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			var gotData []byte

			readFile := func(path string) ([]byte, error) {
				if tc.readFileErr != nil {
					return nil, tc.readFileErr
				}
				return []byte(tc.inputYAML), nil
			}

			writeFile := func(path string, data []byte, perm os.FileMode) error {
				if tc.writeFileErr != nil {
					return tc.writeFileErr
				}
				gotPath = path
				gotData = data
				return nil
			}

			step := NewCreateInstallConfigStep(
				logrus.NewEntry(logrus.StandardLogger()),
				tc.ci,
				buildCmdFunc(t, tc.wantCmdArgs),
				runCmdFunc(tc.runCmdErr),
				WithReadFile(readFile),
				WithWriteFile(writeFile),
			)

			err := step.Run(context.TODO())

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q but got nil", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("expected error %q but got %q", tc.wantErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotPath != tc.wantPath {
				t.Errorf("expected write to %q but got %q", tc.wantPath, gotPath)
			}

			if string(gotData) != tc.wantData {
				t.Errorf("expected data:\n%s\nbut got:\n%s", tc.wantData, string(gotData))
			}
		})
	}
}
