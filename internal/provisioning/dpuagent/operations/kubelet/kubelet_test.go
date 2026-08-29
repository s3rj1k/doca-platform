/*
Copyright 2026 NVIDIA

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubelet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/cmd/dpuagent/opts"
	"github.com/nvidia/doca-platform/internal/provisioning/dpuagent/operations"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type mockClient struct {
	getObjectFunc    func(ctx context.Context, namespace, name string, obj client.Object) error
	updateStatusFunc func(ctx context.Context, status provisioningv1.AgentStatus) error
	healthCheckFunc  func() error
}

func (m *mockClient) GetObject(ctx context.Context, namespace, name string, obj client.Object) error {
	if m.getObjectFunc != nil {
		return m.getObjectFunc(ctx, namespace, name, obj)
	}
	return nil
}

func (m *mockClient) UpdateStatus(ctx context.Context, status provisioningv1.AgentStatus) error {
	if m.updateStatusFunc != nil {
		return m.updateStatusFunc(ctx, status)
	}
	return nil
}

func (m *mockClient) HealthCheck() error {
	if m.healthCheckFunc != nil {
		return m.healthCheckFunc()
	}
	return nil
}

// minimalKubeletConfigYAML is a valid KubeletConfiguration stub for tests.
const minimalKubeletConfigYAML = `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
`

var _ = Describe("Kubelet", func() {
	var tempDir string

	BeforeEach(func() {
		var err error
		tempDir, err = os.MkdirTemp("", "kubelet-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(os.RemoveAll(tempDir)).To(Succeed())
	})

	Context("when built-in kubelet config is present", func() {
		It("should be skipped if SkipRemoveBuiltinKubelet is true", func() {
			operation := &RemoveBuiltinKubelet{}
			Expect(operation.ShouldSkip(&operations.Context{
				Options: opts.Options{
					SkipRemoveBuiltinKubelet: true,
				},
			})).To(BeTrue())
		})

		It("should remove the built-in kubelet config", func() {
			mockFile := filepath.Join(tempDir, "/usr/lib/systemd/system/kubelet.service.d/90-kubelet-bluefield.conf")
			Expect(os.MkdirAll(filepath.Dir(mockFile), 0755)).To(Succeed())
			Expect(os.WriteFile(mockFile, []byte(""), 0644)).To(Succeed())
			_, err := os.Stat(mockFile)
			Expect(err).NotTo(HaveOccurred())
			operation := &RemoveBuiltinKubelet{
				rootFS: tempDir,
				stopKubelet: func() error {
					return nil
				},
			}
			err = operation.Execute(ctx, &operations.Context{})
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(mockFile)
			Expect(os.IsNotExist(err)).To(BeTrue())
		})

		It("should not stop kubelet if the built-in kubelet config is not present", func() {
			mockFile := filepath.Join(tempDir, "/usr/lib/systemd/system/kubelet.service.d/90-kubelet-bluefield.conf")
			_, err := os.Stat(mockFile)
			Expect(os.IsNotExist(err)).To(BeTrue())
			operation := &RemoveBuiltinKubelet{
				rootFS: tempDir,
				stopKubelet: func() error {
					Fail("should not stop kubelet")
					return nil
				},
			}
			err = operation.Execute(ctx, &operations.Context{})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("StartKubelet", func() {
		It("should be skipped if SkipStartKubelet is true", func() {
			operation := &StartKubelet{}
			Expect(operation.ShouldSkip(&operations.Context{
				Options: opts.Options{
					SkipStartKubelet: true,
				},
			})).To(BeTrue())
		})

		It("should disable and start kubelet", func() {
			var executedCmds []string
			operation := &StartKubelet{
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					executedCmds = append(executedCmds, cmd)
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{})
			Expect(err).NotTo(HaveOccurred())
			Expect(executedCmds).To(Equal([]string{"systemctl disable kubelet", "systemctl start kubelet"}))
		})

		It("should return error if disable fails", func() {
			operation := &StartKubelet{
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					var stderr bytes.Buffer
					stderr.WriteString("Failed to disable kubelet.service")
					return bytes.Buffer{}, stderr, fmt.Errorf("exit status 1")
				},
			}
			err := operation.Execute(ctx, &operations.Context{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to disable kubelet"))
		})

		It("should return error if start fails", func() {
			operation := &StartKubelet{
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					if cmd == "systemctl disable kubelet" {
						return bytes.Buffer{}, bytes.Buffer{}, nil
					}
					var stderr bytes.Buffer
					stderr.WriteString("Failed to start kubelet.service")
					return bytes.Buffer{}, stderr, fmt.Errorf("exit status 5")
				},
			}
			err := operation.Execute(ctx, &operations.Context{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to start kubelet"))
		})
	})

	Context("ConfigureKubelet", func() {
		It("should be skipped if SkipConfigureKubelet is true", func() {
			operation := &ConfigureKubelet{}
			Expect(operation.ShouldSkip(&operations.Context{
				Options: opts.Options{
					SkipConfigureKubelet: true,
				},
			})).To(BeTrue())
		})

		It("should return error if LatestDPU is nil", func() {
			operation := &ConfigureKubelet{}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: nil,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("latest DPU not retrieved"))
		})

		It("should skip if kubelet is already configured", func() {
			stopKubeletCalled := false
			operation := &ConfigureKubelet{
				stopKubelet: func() error {
					stopKubeletCalled = true
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{
					Status: provisioningv1.DPUStatus{
						AgentStatus: &provisioningv1.AgentStatus{
							Conditions: []metav1.Condition{
								{
									Type:   ConditionType,
									Status: metav1.ConditionTrue,
								},
							},
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(stopKubeletCalled).To(BeFalse())
		})

		It("should return error if GetObject fails", func() {
			operation := &ConfigureKubelet{
				caPath:           filepath.Join(tempDir, "ca.crt"),
				bootstrapPath:    filepath.Join(tempDir, "bootstrap.conf"),
				systemdDropInDir: filepath.Join(tempDir, "systemd"),
				stopKubelet: func() error {
					return nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					return fmt.Errorf("failed to get secret")
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get kubeadm secret"))
		})

		It("should return error if secret does not contain join key", func() {
			operation := &ConfigureKubelet{
				caPath:           filepath.Join(tempDir, "ca.crt"),
				bootstrapPath:    filepath.Join(tempDir, "bootstrap.conf"),
				systemdDropInDir: filepath.Join(tempDir, "systemd"),
				stopKubelet: func() error {
					return nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					secret := obj.(*corev1.Secret)
					secret.Data = map[string][]byte{
						"other-key": []byte("some-value"),
					}
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not contain key"))
		})

		It("should return error if join command fails", func() {
			operation := &ConfigureKubelet{
				caPath:           filepath.Join(tempDir, "ca.crt"),
				bootstrapPath:    filepath.Join(tempDir, "bootstrap.conf"),
				systemdDropInDir: filepath.Join(tempDir, "systemd"),
				stopKubelet: func() error {
					return nil
				},
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("join failed")
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					secret := obj.(*corev1.Secret)
					secret.Data = map[string][]byte{
						"join": []byte("kubeadm join --token xxx"),
					}
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to run join command"))
		})

		It("should successfully execute join command", func() {
			joinCmdExecuted := ""
			systemdDropInDir := filepath.Join(tempDir, "systemd")
			kubeletDataConfig := filepath.Join(tempDir, "config.yaml")
			Expect(os.WriteFile(kubeletDataConfig, []byte(minimalKubeletConfigYAML), 0644)).To(Succeed())

			operation := &ConfigureKubelet{
				caPath:            filepath.Join(tempDir, "ca.crt"),
				bootstrapPath:     filepath.Join(tempDir, "bootstrap.conf"),
				kubeletDataConfig: kubeletDataConfig,
				systemdDropInDir:  systemdDropInDir,
				stopKubelet: func() error {
					return nil
				},
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					if cmd == kubeletVersionCmd {
						var stdout bytes.Buffer
						stdout.WriteString("Kubernetes v1.33.3")
						return stdout, bytes.Buffer{}, nil
					}
					joinCmdExecuted = cmd
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					Expect(namespace).To(Equal("kube-system"))
					Expect(name).To(Equal("kubeadm-join-secret"))
					secret := obj.(*corev1.Secret)
					secret.Data = map[string][]byte{
						"join": []byte("kubeadm join 10.0.0.1:6443 --token abcdef"),
					}
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "kube-system",
					KubeadmSecretName:      "kubeadm-join-secret",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying join command was executed")
			Expect(joinCmdExecuted).To(Equal("kubeadm join 10.0.0.1:6443 --token abcdef"))

			By("verifying systemd drop-in directory was created")
			info, err := os.Stat(systemdDropInDir)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.IsDir()).To(BeTrue())

			By("verifying systemd drop-in file was created with correct content")
			dropInPath := filepath.Join(systemdDropInDir, kubeletSystemdDropInFile)
			content, err := os.ReadFile(dropInPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal(kubeletSystemdDropInConfig))
		})

		It("should set all hardening fields and keep existing settings in kubelet config", func() {
			kubeletDataConfig := filepath.Join(tempDir, "config.yaml")
			initial := minimalKubeletConfigYAML + "clusterDNS:\n- 10.96.0.10\n"
			Expect(os.WriteFile(kubeletDataConfig, []byte(initial), 0644)).To(Succeed())

			operation := &ConfigureKubelet{kubeletDataConfig: kubeletDataConfig}
			Expect(operation.addKubeletCustomizedConfig()).To(Succeed())

			merged, err := os.ReadFile(kubeletDataConfig)
			Expect(err).NotTo(HaveOccurred())
			cfg := &kubeletconfigv1beta1.KubeletConfiguration{}
			Expect(yaml.Unmarshal(merged, cfg)).To(Succeed())
			Expect(cfg.ProtectKernelDefaults).To(BeTrue())
			Expect(cfg.SeccompDefault).NotTo(BeNil())
			Expect(*cfg.SeccompDefault).To(BeTrue())
			Expect(cfg.StreamingConnectionIdleTimeout.Duration).To(Equal(5 * time.Minute))
			Expect(cfg.EventRecordQPS).NotTo(BeNil())
			Expect(*cfg.EventRecordQPS).To(Equal(int32(50)))
			Expect(cfg.ClusterDNS).To(Equal([]string{"10.96.0.10"}))
		})

		It("should return an error if the kubelet config file does not exist", func() {
			missing := filepath.Join(tempDir, "nonexistent-config.yaml")
			operation := &ConfigureKubelet{kubeletDataConfig: missing}
			err := operation.addKubeletCustomizedConfig()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should return an error if the kubelet config file is not valid YAML", func() {
			kubeletDataConfig := filepath.Join(tempDir, "bad.yaml")
			Expect(os.WriteFile(kubeletDataConfig, []byte("{invalid"), 0644)).To(Succeed())
			operation := &ConfigureKubelet{kubeletDataConfig: kubeletDataConfig}
			err := operation.addKubeletCustomizedConfig()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("parse kubelet config"))
		})

		It("should clean up before joining", func() {
			caPath := filepath.Join(tempDir, "ca.crt")
			bootstrapPath := filepath.Join(tempDir, "bootstrap.conf")
			kubeletConfPath := filepath.Join(tempDir, "kubelet.conf")

			By("creating existing CA, bootstrap and kubelet config files")
			Expect(os.WriteFile(caPath, []byte("old-ca"), 0644)).To(Succeed())
			Expect(os.WriteFile(bootstrapPath, []byte("old-bootstrap"), 0644)).To(Succeed())
			Expect(os.WriteFile(kubeletConfPath, []byte("old-kubelet-config"), 0644)).To(Succeed())

			kubeletDataConfig := filepath.Join(tempDir, "config.yaml")
			Expect(os.WriteFile(kubeletDataConfig, []byte(minimalKubeletConfigYAML), 0644)).To(Succeed())

			stopKubeletCalled := false
			operation := &ConfigureKubelet{
				caPath:            caPath,
				bootstrapPath:     bootstrapPath,
				kubeletConfPath:   kubeletConfPath,
				kubeletDataConfig: kubeletDataConfig,
				systemdDropInDir:  filepath.Join(tempDir, "systemd"),
				stopKubelet: func() error {
					stopKubeletCalled = true
					return nil
				},
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					if cmd == kubeletVersionCmd {
						var stdout bytes.Buffer
						stdout.WriteString("Kubernetes v1.33.3")
						return stdout, bytes.Buffer{}, nil
					}
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					secret := obj.(*corev1.Secret)
					secret.Data = map[string][]byte{
						"join": []byte("kubeadm join"),
					}
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying stopKubelet was called")
			Expect(stopKubeletCalled).To(BeTrue())

			By("verifying CA and bootstrap files are removed")
			_, err = os.Stat(caPath)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(bootstrapPath)
			Expect(os.IsNotExist(err)).To(BeTrue())
			_, err = os.Stat(kubeletConfPath)
			Expect(os.IsNotExist(err)).To(BeTrue())
		})

		It("should run the join but skip every kubelet service step when granular skips are set", func() {
			caPath := filepath.Join(tempDir, "granular-ca.crt")
			systemdDropInDir := filepath.Join(tempDir, "granular-systemd")
			Expect(os.WriteFile(caPath, []byte("keep-me"), 0644)).To(Succeed())

			stopKubeletCalled := false
			joinCmdExecuted := ""
			operation := &ConfigureKubelet{
				caPath:           caPath,
				bootstrapPath:    filepath.Join(tempDir, "granular-bootstrap.conf"),
				kubeletConfPath:  filepath.Join(tempDir, "granular-kubelet.conf"),
				systemdDropInDir: systemdDropInDir,
				stopKubelet: func() error {
					stopKubeletCalled = true
					return nil
				},
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					if cmd == kubeletVersionCmd {
						return bytes.Buffer{}, bytes.Buffer{}, fmt.Errorf("kubelet binary should not be queried")
					}
					joinCmdExecuted = cmd
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					secret := obj.(*corev1.Secret)
					secret.Data = map[string][]byte{
						"join": []byte("/opt/dpf/k0s-join.sh"),
					}
					return nil
				},
			}
			err := operation.Execute(ctx, &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace:      "default",
					KubeadmSecretName:           "kubeadm-join",
					SkipKubeletConfigCleanup:    true,
					SkipKubeletStop:             true,
					SkipKubeletSystemdDropIn:    true,
					SkipKubeletCustomizedConfig: true,
					SkipKubeletVersionCheck:     true,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the join payload still ran")
			Expect(joinCmdExecuted).To(Equal("/opt/dpf/k0s-join.sh"))

			By("verifying no kubelet service step was touched")
			Expect(stopKubeletCalled).To(BeFalse())
			_, err = os.Stat(caPath)
			Expect(err).NotTo(HaveOccurred())
			_, err = os.Stat(systemdDropInDir)
			Expect(os.IsNotExist(err)).To(BeTrue())
		})

		// One skip at a time, because setting all five together cannot tell a guard reading
		// the wrong Options field from one reading the right one.
		DescribeTable("should skip only the step its own option names",
			func(setSkip func(*opts.Options), expect func(effects)) {
				caPath := filepath.Join(tempDir, "single-ca.crt")
				bootstrapPath := filepath.Join(tempDir, "single-bootstrap.conf")
				systemdDropInDir := filepath.Join(tempDir, "single-systemd")
				kubeletDataConfig := filepath.Join(tempDir, "single-config.yaml")
				Expect(os.WriteFile(caPath, []byte("keep-me"), 0644)).To(Succeed())
				Expect(os.WriteFile(bootstrapPath, []byte("keep-me"), 0644)).To(Succeed())
				Expect(os.WriteFile(kubeletDataConfig, []byte(minimalKubeletConfigYAML), 0644)).To(Succeed())

				got := effects{}
				operation := &ConfigureKubelet{
					caPath:            caPath,
					bootstrapPath:     bootstrapPath,
					kubeletConfPath:   filepath.Join(tempDir, "single-kubelet.conf"),
					kubeletDataConfig: kubeletDataConfig,
					systemdDropInDir:  systemdDropInDir,
					stopKubelet: func() error {
						got.stopCalled = true
						return nil
					},
					runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
						if cmd == kubeletVersionCmd {
							got.versionQueried = true
							var stdout bytes.Buffer
							stdout.WriteString("Kubernetes v1.33.3")
							return stdout, bytes.Buffer{}, nil
						}
						got.joinCmd = cmd
						return bytes.Buffer{}, bytes.Buffer{}, nil
					},
				}
				mockCli := &mockClient{
					getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
						obj.(*corev1.Secret).Data = map[string][]byte{"join": []byte("/opt/dpf/k0s-join.sh")}
						return nil
					},
				}

				options := opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				}
				setSkip(&options)

				opCtx := &operations.Context{
					LatestDPU: &provisioningv1.DPU{},
					Client:    mockCli,
					Options:   options,
				}
				Expect(operation.Execute(ctx, opCtx)).To(Succeed())

				got.caSurvived = fileExists(caPath)
				got.bootstrapSurvived = fileExists(bootstrapPath)
				got.dropInWritten = fileExists(systemdDropInDir)
				got.reportedVersion = opCtx.Status.KubeletVersion

				// Hardening is only written back when the customized config step runs.
				hardened, err := os.ReadFile(kubeletDataConfig)
				Expect(err).NotTo(HaveOccurred())
				got.configHardened = strings.Contains(string(hardened), "protectKernelDefaults: true")

				By("verifying the join payload always runs")
				Expect(got.joinCmd).To(Equal("/opt/dpf/k0s-join.sh"))
				expect(got)
			},
			Entry("config cleanup", func(o *opts.Options) { o.SkipKubeletConfigCleanup = true }, func(got effects) {
				Expect(got.caSurvived).To(BeTrue(), "ca should be left in place")
				Expect(got.bootstrapSurvived).To(BeTrue(), "bootstrap kubeconfig should be left in place")
				Expect(got.stopCalled).To(BeTrue(), "kubelet should still be stopped")
				Expect(got.dropInWritten).To(BeTrue(), "drop-in should still be written")
				Expect(got.versionQueried).To(BeTrue(), "version should still be checked")
				Expect(got.configHardened).To(BeTrue(), "kubelet config should still be hardened")
			}),
			Entry("stop", func(o *opts.Options) { o.SkipKubeletStop = true }, func(got effects) {
				Expect(got.stopCalled).To(BeFalse(), "kubelet should not be stopped")
				Expect(got.caSurvived).To(BeFalse(), "ca should still be removed")
				Expect(got.dropInWritten).To(BeTrue(), "drop-in should still be written")
				Expect(got.versionQueried).To(BeTrue(), "version should still be checked")
				Expect(got.configHardened).To(BeTrue(), "kubelet config should still be hardened")
			}),
			Entry("systemd drop-in", func(o *opts.Options) { o.SkipKubeletSystemdDropIn = true }, func(got effects) {
				Expect(got.dropInWritten).To(BeFalse(), "drop-in should not be written")
				Expect(got.caSurvived).To(BeFalse(), "ca should still be removed")
				Expect(got.stopCalled).To(BeTrue(), "kubelet should still be stopped")
				Expect(got.versionQueried).To(BeTrue(), "version should still be checked")
				Expect(got.configHardened).To(BeTrue(), "kubelet config should still be hardened")
			}),
			Entry("customized config", func(o *opts.Options) { o.SkipKubeletCustomizedConfig = true }, func(got effects) {
				Expect(got.caSurvived).To(BeFalse(), "ca should still be removed")
				Expect(got.stopCalled).To(BeTrue(), "kubelet should still be stopped")
				Expect(got.dropInWritten).To(BeTrue(), "drop-in should still be written")
				Expect(got.versionQueried).To(BeTrue(), "version should still be checked")
				Expect(got.configHardened).To(BeFalse(), "kubelet config should not be hardened")
			}),
			Entry("version check", func(o *opts.Options) { o.SkipKubeletVersionCheck = true }, func(got effects) {
				Expect(got.versionQueried).To(BeFalse(), "kubelet binary should not be queried")
				Expect(got.reportedVersion).To(BeNil(), "no version should be reported")
				Expect(got.caSurvived).To(BeFalse(), "ca should still be removed")
				Expect(got.stopCalled).To(BeTrue(), "kubelet should still be stopped")
				Expect(got.dropInWritten).To(BeTrue(), "drop-in should still be written")
				Expect(got.configHardened).To(BeTrue(), "kubelet config should still be hardened")
			}),
		)

		It("should report the kubelet version when the check is not skipped", func() {
			kubeletDataConfig := filepath.Join(tempDir, "reported-config.yaml")
			Expect(os.WriteFile(kubeletDataConfig, []byte(minimalKubeletConfigYAML), 0644)).To(Succeed())

			operation := &ConfigureKubelet{
				caPath:            filepath.Join(tempDir, "reported-ca.crt"),
				bootstrapPath:     filepath.Join(tempDir, "reported-bootstrap.conf"),
				kubeletConfPath:   filepath.Join(tempDir, "reported-kubelet.conf"),
				kubeletDataConfig: kubeletDataConfig,
				systemdDropInDir:  filepath.Join(tempDir, "reported-systemd"),
				stopKubelet:       func() error { return nil },
				runBash: func(cmd string) (bytes.Buffer, bytes.Buffer, error) {
					if cmd == kubeletVersionCmd {
						var stdout bytes.Buffer
						stdout.WriteString("Kubernetes v1.33.3")
						return stdout, bytes.Buffer{}, nil
					}
					return bytes.Buffer{}, bytes.Buffer{}, nil
				},
			}
			mockCli := &mockClient{
				getObjectFunc: func(ctx context.Context, namespace, name string, obj client.Object) error {
					obj.(*corev1.Secret).Data = map[string][]byte{"join": []byte("join")}
					return nil
				},
			}

			opCtx := &operations.Context{
				LatestDPU: &provisioningv1.DPU{},
				Client:    mockCli,
				Options: opts.Options{
					KubeadmSecretNamespace: "default",
					KubeadmSecretName:      "kubeadm-join",
				},
			}
			Expect(operation.Execute(ctx, opCtx)).To(Succeed())
			Expect(opCtx.Status.KubeletVersion).NotTo(BeNil())
			Expect(*opCtx.Status.KubeletVersion).To(Equal("v1.33.3"))
		})

	})
})

// effects records what ConfigureKubelet did, so each skip can assert on the steps it did
// not name as well as the one it did.
type effects struct {
	joinCmd           string
	stopCalled        bool
	versionQueried    bool
	caSurvived        bool
	bootstrapSurvived bool
	dropInWritten     bool
	configHardened    bool
	reportedVersion   *string
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
