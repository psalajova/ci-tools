package prowgen

import (
	"testing"
	"time"

	"k8s.io/utils/pointer"
	prowv1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"

	ciop "github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/testhelper"
)

func TestSparseCheckoutFiles(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name     string
		config   *ciop.ReleaseBuildConfiguration
		expected []string
	}{
		{
			name:     "no build root and no images: empty",
			config:   &ciop.ReleaseBuildConfiguration{},
			expected: nil,
		},
		{
			name: "from_repository set: includes .ci-operator.yaml",
			config: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					BuildRootImage: &ciop.BuildRootImageConfiguration{FromRepository: true},
				},
			},
			expected: []string{".ci-operator.yaml"},
		},
		{
			name: "from_repository not set: no .ci-operator.yaml",
			config: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					BuildRootImage: &ciop.BuildRootImageConfiguration{
						ProjectImageBuild: &ciop.ProjectDirectoryImageBuildInputs{},
					},
				},
			},
			expected: nil,
		},
		{
			name: "image with default Dockerfile",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{}},
				}},
			},
			expected: []string{"Dockerfile"},
		},
		{
			name: "image with custom DockerfilePath",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfilePath: "images/tool.Dockerfile"}},
				}},
			},
			expected: []string{"images/tool.Dockerfile"},
		},
		{
			name: "image with ContextDir and DockerfilePath",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "cmd/tool", DockerfilePath: "build.Dockerfile"}},
				}},
			},
			expected: []string{"cmd/tool/build.Dockerfile"},
		},
		{
			name: "image with ContextDir only: defaults to Dockerfile",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "images/app"}},
				}},
			},
			expected: []string{"images/app/Dockerfile"},
		},
		{
			name: "image with DockerfileLiteral: skipped",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfileLiteral: pointer.StringPtr("FROM scratch")}},
				}},
			},
			expected: nil,
		},
		{
			name: "image with Ref: skipped",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image", Ref: "some-ref"},
				}},
			},
			expected: nil,
		},
		{
			name: "from_repository with multiple images: combines all",
			config: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					BuildRootImage: &ciop.BuildRootImageConfiguration{FromRepository: true},
				},
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image-default", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{}},
					{To: "image-custom", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfilePath: "images/tool.Dockerfile"}},
					{To: "image-ctx", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "images/app"}},
					{To: "image-inline", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfileLiteral: pointer.StringPtr("FROM scratch")}},
					{To: "image-ref", Ref: "some-ref"},
				}},
			},
			expected: []string{".ci-operator.yaml", "Dockerfile", "images/app/Dockerfile", "images/tool.Dockerfile"},
		},
		{
			name: "duplicate Dockerfile paths are deduplicated",
			config: &ciop.ReleaseBuildConfiguration{
				Images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
					{To: "image-1", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{}},
					{To: "image-2", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{}},
					{To: "image-3", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "cmd/app"}},
					{To: "image-4", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "cmd/app"}},
				}},
			},
			expected: []string{"Dockerfile", "cmd/app/Dockerfile"},
		},
		{
			name: "BuildRootImages map with from_repository",
			config: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					BuildRootImages: map[string]ciop.BuildRootImageConfiguration{
						"go-1.21": {FromRepository: true},
					},
				},
			},
			expected: []string{".ci-operator.yaml"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := sparseCheckoutFiles(tc.config)
			if len(tc.expected) == 0 && len(result) == 0 {
				return
			}
			if len(result) != len(tc.expected) {
				t.Fatalf("expected %v, got %v", tc.expected, result)
			}
			for i := range tc.expected {
				if result[i] != tc.expected[i] {
					t.Errorf("index %d: expected %q, got %q", i, tc.expected[i], result[i])
				}
			}
		})
	}
}

func TestProwJobBaseBuilder(t *testing.T) {
	defaultInfo := &ProwgenInfo{
		Metadata: ciop.Metadata{
			Org:    "org",
			Repo:   "repo",
			Branch: "branch",
		},
	}
	t.Parallel()
	testCases := []struct {
		name string

		inputs           ciop.InputConfiguration
		images           ciop.ImageConfiguration
		binCommand       string
		testBinCommand   string
		prowgenOverrides *ciop.ProwgenOverrides

		podSpecBuilder CiOperatorPodSpecGenerator
		info           *ProwgenInfo
		prefix         string
	}{
		{
			name:           "default job without further configuration",
			info:           defaultInfo,
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name:           "job with configured prefix",
			info:           defaultInfo,
			prefix:         "prefix",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with a variant",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch", Variant: "variant"},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with latest release that is a candidate: has `job-release` label",
			info: defaultInfo,
			inputs: ciop.InputConfiguration{
				Releases: map[string]ciop.UnresolvedRelease{ciop.LatestReleaseName: {Candidate: &ciop.Candidate{Version: "THIS"}}},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with not a latest release that is a candidate: does not have `job-release` label",
			info: defaultInfo,
			inputs: ciop.InputConfiguration{
				Releases: map[string]ciop.UnresolvedRelease{ciop.InitialReleaseName: {Candidate: &ciop.Candidate{Version: "THIS"}}},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with latest release that is not a candidate: does not have `job-release` label",
			info: defaultInfo,
			inputs: ciop.InputConfiguration{
				Releases: map[string]ciop.UnresolvedRelease{ciop.LatestReleaseName: {Release: &ciop.Release{Version: "THIS"}}},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name:           "job with no builds outside of openshift/release@main: does not have `no-builds` label",
			info:           defaultInfo,
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with no builds in openshift/release@main: does have `no-builds` label",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "openshift", Repo: "release", Branch: "main"},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with a buildroot in of openshift/release@main: does not have `no-builds` label",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "openshift", Repo: "release", Branch: "main"},
			},
			inputs: ciop.InputConfiguration{
				BuildRootImage: &ciop.BuildRootImageConfiguration{
					ProjectImageBuild: &ciop.ProjectDirectoryImageBuildInputs{},
				},
			},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with binary build in openshift/release@main: does not have `no-builds` label",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "openshift", Repo: "release", Branch: "main"},
			},
			binCommand:     "make",
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with test binary build in of openshift/release@main: does not have `no-builds` label",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "openshift", Repo: "release", Branch: "main"},
			},
			testBinCommand: "make test",
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name: "job with image builds in of openshift/release@main: does not have `no-builds` label",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "openshift", Repo: "release", Branch: "main"},
			},
			images:         ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{{From: "base", To: "image"}}},
			prefix:         "default",
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name:           "default job without further configuration, including podspec",
			info:           defaultInfo,
			prefix:         "default",
			podSpecBuilder: NewCiOperatorPodSpecGenerator(),
		},
		{
			name: "job with a variant, including podspec",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch", Variant: "variant"},
			},
			prefix:         "default",
			podSpecBuilder: NewCiOperatorPodSpecGenerator(),
		},
		{
			name: "private job without cloning, including podspec",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch"},
				Config:   config.Prowgen{Private: true},
			},
			prefix:         "default",
			podSpecBuilder: NewCiOperatorPodSpecGenerator(),
		},
		{
			name: "private job with cloning, including podspec",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch"},
				Config:   config.Prowgen{Private: true},
			},
			prefix: "default",
			inputs: ciop.InputConfiguration{
				BuildRootImage: &ciop.BuildRootImageConfiguration{FromRepository: true},
			},
			podSpecBuilder: NewCiOperatorPodSpecGenerator(),
		},
		{
			name:             "private job via ci-operator config",
			info:             &ProwgenInfo{Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch"}},
			prowgenOverrides: &ciop.ProwgenOverrides{Private: true},
			prefix:           "default",
			podSpecBuilder:   NewCiOperatorPodSpecGenerator(),
		},
		{
			name:             "private job with expose via ci-operator config",
			info:             &ProwgenInfo{Metadata: ciop.Metadata{Org: "vorg", Repo: "vrepo", Branch: "vbranch"}},
			prowgenOverrides: &ciop.ProwgenOverrides{Private: true, Expose: true},
			prefix:           "default",
			podSpecBuilder:   NewCiOperatorPodSpecGenerator(),
		},
		{
			name:   "job with from_repository build root: has sparse checkout with .ci-operator.yaml",
			info:   defaultInfo,
			prefix: "default",
			inputs: ciop.InputConfiguration{
				BuildRootImage: &ciop.BuildRootImageConfiguration{FromRepository: true},
			},
			podSpecBuilder: newFakePodSpecBuilder(),
		},
		{
			name:   "job with from_repository build root and images: adds dockerfiles to sparse checkout",
			info:   defaultInfo,
			prefix: "default",
			inputs: ciop.InputConfiguration{
				BuildRootImage: &ciop.BuildRootImageConfiguration{FromRepository: true},
			},
			images: ciop.ImageConfiguration{Items: []ciop.ProjectDirectoryImageBuildStepConfiguration{
				{To: "image-default", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{}},
				{To: "image-custom-df", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfilePath: "images/tool.Dockerfile"}},
				{To: "image-with-ctx", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "cmd/tool", DockerfilePath: "build.Dockerfile"}},
				{To: "image-ctx-only", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{ContextDir: "images/app"}},
				{To: "image-inline", ProjectDirectoryImageBuildInputs: ciop.ProjectDirectoryImageBuildInputs{DockerfileLiteral: pointer.StringPtr("FROM scratch")}},
			}},
			podSpecBuilder: newFakePodSpecBuilder(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			t.Parallel()
			ciopconfig := &ciop.ReleaseBuildConfiguration{
				InputConfiguration:      tc.inputs,
				Images:                  tc.images,
				BinaryBuildCommands:     tc.binCommand,
				TestBinaryBuildCommands: tc.testBinCommand,
				Metadata:                tc.info.Metadata,
				Prowgen:                 tc.prowgenOverrides,
			}
			b := NewProwJobBaseBuilder(ciopconfig, tc.info, tc.podSpecBuilder).Build(tc.prefix)
			testhelper.CompareWithFixture(t, b)
		})
	}
}

func TestGenerateJobBase(t *testing.T) {
	var testCases = []struct {
		testName              string
		name                  string
		info                  *ProwgenInfo
		canonicalGoRepository string
		rehearsable           bool
	}{
		{
			testName: "no special options",
			name:     "test",
			info:     &ProwgenInfo{Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch"}},
		},
		{
			testName:    "rehearsable",
			name:        "test",
			info:        &ProwgenInfo{Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch"}},
			rehearsable: true,
		},
		{
			testName: "config variant",
			name:     "test",
			info:     &ProwgenInfo{Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch", Variant: "whatever"}},
		},
		{
			testName:              "path alias",
			name:                  "test",
			canonicalGoRepository: "/some/where",
			info:                  &ProwgenInfo{Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch", Variant: "whatever"}},
		},
		{
			testName: "hidden job for private repos",
			name:     "test",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch"},
				Config:   config.Prowgen{Private: true},
			},
		},
		{
			testName: "expose job for private repos with public results",
			name:     "test",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch"},
				Config:   config.Prowgen{Private: true, Expose: true},
			},
		},
		{
			testName: "expose option set but not private",
			name:     "test",
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "org", Repo: "repo", Branch: "branch"},
				Config:   config.Prowgen{Private: false, Expose: true},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.testName, func(t *testing.T) {
			jobBaseGen := NewProwJobBaseBuilder(&ciop.ReleaseBuildConfiguration{CanonicalGoRepository: &testCase.canonicalGoRepository}, testCase.info, newFakePodSpecBuilder()).Rehearsable(testCase.rehearsable).TestName(testCase.name)
			testhelper.CompareWithFixture(t, jobBaseGen.Build("pull"))
		})
	}
}

func TestNewProwJobBaseBuilderForTest(t *testing.T) {
	ciopconfig := &ciop.ReleaseBuildConfiguration{}
	defaultInfo := &ProwgenInfo{Metadata: ciop.Metadata{Org: "o", Repo: "r", Branch: "b"}}
	testCases := []struct {
		name string

		cfg  *ciop.ReleaseBuildConfiguration
		test ciop.TestStepConfiguration
		info *ProwgenInfo
	}{
		{
			name: "simple container-based test",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
			},
			info: defaultInfo,
		},
		{
			name: "simple container-based test with timeout",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
				Timeout:                    &v1.Duration{Duration: time.Second},
			},
			info: defaultInfo,
		},
		{
			name: "simple container-based test with timeout and no decoration",
			cfg: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					BuildRootImage: &ciop.BuildRootImageConfiguration{
						FromRepository: true,
					},
				},
			},
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
				Timeout:                    &v1.Duration{Duration: time.Second},
			},
			info: defaultInfo,
		},
		{
			name: "simple container-based test with secret",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
				Secret:                     &ciop.Secret{Name: "s", MountPath: "/path"},
			},
			info: defaultInfo,
		},
		{
			name: "simple container-based test with secrets",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
				Secrets:                    []*ciop.Secret{{Name: "s", MountPath: "/path"}, {Name: "s2", MountPath: "/path2"}},
			},
			info: defaultInfo,
		},
		{
			name: "multi-stage test",
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					Workflow: pointer.StringPtr("workflow"),
				},
			},
			info: defaultInfo,
		},
		{
			name: "multi-stage test with CSI enabled",
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					Workflow: pointer.StringPtr("workflow"),
				},
			},
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "o", Repo: "r", Branch: "b"},
				Config:   config.Prowgen{EnableSecretsStoreCSIDriver: true},
			},
		},
		{
			name: "simple test with CSI enabled",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
			},
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "o", Repo: "r", Branch: "b"},
				Config:   config.Prowgen{EnableSecretsStoreCSIDriver: true},
			},
		},
		{
			name: "multi-stage test with CSI enabled via ci-operator config",
			cfg: &ciop.ReleaseBuildConfiguration{
				Prowgen: &ciop.ProwgenOverrides{EnableSecretsStoreCSIDriver: true},
			},
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					Workflow: pointer.StringPtr("workflow"),
				},
			},
			info: defaultInfo,
		},
		{
			name: "multi-stage test with claim",
			test: ciop.TestStepConfiguration{
				As:           "simple",
				ClusterClaim: &ciop.ClusterClaim{Product: "ocp"},
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					Workflow: pointer.StringPtr("workflow"),
				},
			},
			info: defaultInfo,
		},
		{
			name: "multi-stage test with cluster_profile",
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					ClusterProfile: ciop.ClusterProfileAlibabaCloud,
					Workflow:       pointer.StringPtr("workflow"),
				},
			},
			info: defaultInfo,
		},
		{
			name: "multi-stage test with releases",
			cfg: &ciop.ReleaseBuildConfiguration{
				InputConfiguration: ciop.InputConfiguration{
					Releases: map[string]ciop.UnresolvedRelease{
						"latest": {
							Candidate: &ciop.Candidate{
								ReleaseDescriptor: ciop.ReleaseDescriptor{
									Product: "ocp",
								},
							},
						},
					}},
			},
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfiguration: &ciop.MultiStageTestConfiguration{
					Workflow: pointer.StringPtr("workflow"),
				},
			},
			info: defaultInfo,
		},
		{
			name: "literal multi-stage test",
			test: ciop.TestStepConfiguration{
				As: "simple",
				MultiStageTestConfigurationLiteral: &ciop.MultiStageTestConfigurationLiteral{
					Test: []ciop.LiteralTestStep{{As: "step", From: "src"}},
				},
			},
			info: defaultInfo,
		},
		{
			name: "simple container-based test with cluster",
			test: ciop.TestStepConfiguration{
				As:                         "simple",
				Commands:                   "make",
				Cluster:                    "build01",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
			},
			info: defaultInfo,
		},
		{
			name: "simple with slack reporter config",
			test: ciop.TestStepConfiguration{
				As:                         "unit",
				Commands:                   "make unit",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
			},
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "o", Repo: "r", Branch: "b"},
				Config: config.Prowgen{
					SlackReporterConfigs: []config.SlackReporterConfig{
						{
							Channel:           "some-channel",
							JobStatesToReport: []prowv1.ProwJobState{"error"},
							ReportTemplate:    "some template",
							JobNames:          []string{"unit", "e2e"},
						},
					},
				},
			},
		},
		{
			name: "job excluded by patterns should not have slack reporter config",
			test: ciop.TestStepConfiguration{
				As:                         "unit-skip",
				Commands:                   "make unit",
				ContainerTestConfiguration: &ciop.ContainerTestConfiguration{From: "src"},
			},
			info: &ProwgenInfo{
				Metadata: ciop.Metadata{Org: "o", Repo: "r", Branch: "b"},
				Config: config.Prowgen{
					SlackReporterConfigs: []config.SlackReporterConfig{
						{
							Channel:             "some-channel",
							JobStatesToReport:   []prowv1.ProwJobState{"error"},
							ReportTemplate:      "some template",
							JobNames:            []string{"unit-skip", "e2e"},
							ExcludedJobPatterns: []string{".*-skip$"},
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			t.Parallel()
			if tc.cfg == nil {
				tc.cfg = ciopconfig
			}
			b := NewProwJobBaseBuilderForTest(tc.cfg, tc.info, NewCiOperatorPodSpecGenerator(), tc.test).Build("prefix")
			testhelper.CompareWithFixture(t, b)
		})
	}
}

func TestMiscellaneous(t *testing.T) {
	defaultInfo := &ProwgenInfo{
		Metadata: ciop.Metadata{
			Org:    "org",
			Repo:   "repo",
			Branch: "branch",
		},
	}
	defaultConfig := &ciop.ReleaseBuildConfiguration{
		Metadata: defaultInfo.Metadata,
	}
	simpleBuilder := func() *prowJobBaseBuilder {
		return NewProwJobBaseBuilder(defaultConfig, defaultInfo, newFakePodSpecBuilder())
	}

	t.Parallel()
	testCases := []struct {
		name    string
		builder *prowJobBaseBuilder
	}{
		{
			name:    "WithLabel",
			builder: simpleBuilder().WithLabel("key", "value"),
		},
		{
			name:    "Cluster",
			builder: simpleBuilder().Cluster("build99"),
		},
		{
			name:    "TestName",
			builder: simpleBuilder().TestName("best-test"),
		},
		{
			name:    "Rehearsable",
			builder: simpleBuilder().Rehearsable(true),
		},
		{
			name:    "PathAlias",
			builder: simpleBuilder().PathAlias("alias.path"),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			t.Parallel()
			jobBase := tc.builder.Build("default")
			testhelper.CompareWithFixture(t, jobBase)
		})
	}
}
