/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2022 Red Hat, Inc.
 *
 */

package storage

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"kubevirt.io/kubevirt/tests/util"

	k8sv1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	exportv1 "kubevirt.io/api/export/v1alpha1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/kubevirt/tests"
	cd "kubevirt.io/kubevirt/tests/containerdisk"
)

const (
	caBundleKey = "ca-bundle"
	caCertPath  = "/cacerts"
	dataPath    = "/data"

	// annContentType is an annotation on a PVC indicating the content type. This is populated by CDI.
	annContentType = "cdi.kubevirt.io/storage.contentType"

	kubevirtcontentUrlTemplate   = "%s?x-kubevirt-export-token=%s"
	archiveDircontentUrlTemplate = "%s/disk.img?x-kubevirt-export-token=%s"
)

var _ = SIGDescribe("Export", func() {
	var err error
	var token *k8sv1.Secret
	var virtClient kubecli.KubevirtClient

	BeforeEach(func() {
		virtClient, err = kubecli.GetKubevirtClient()
		util.PanicOnError(err)

		tests.BeforeTestCleanup()
	})

	AfterEach(func() {
		err := virtClient.CoreV1().Secrets(token.Namespace).Delete(context.Background(), token.Name, metav1.DeleteOptions{})
		Expect(err).ToNot(HaveOccurred())
	})

	createDownloadPodForPvc := func(pvc *k8sv1.PersistentVolumeClaim, caConfigMap *k8sv1.ConfigMap) *k8sv1.Pod {
		volumeName := pvc.GetName()
		podName := "download-pod"
		pod := tests.RenderPod(podName, []string{"/bin/sh", "-c", "sleep 360"}, []string{})
		pod.Spec.Volumes = append(pod.Spec.Volumes, k8sv1.Volume{
			Name: volumeName,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.GetName(),
				},
			},
		}, k8sv1.Volume{
			Name: "cacerts",
			VolumeSource: k8sv1.VolumeSource{
				ConfigMap: &k8sv1.ConfigMapVolumeSource{
					LocalObjectReference: k8sv1.LocalObjectReference{
						Name: caConfigMap.Name,
					},
				},
			},
		})

		volumeMode := pvc.Spec.VolumeMode
		if volumeMode != nil && *volumeMode == k8sv1.PersistentVolumeBlock {
			pod.Spec.Containers[0].VolumeDevices = append(pod.Spec.Containers[0].VolumeDevices, k8sv1.VolumeDevice{
				Name:       volumeName,
				DevicePath: "/dev/volume",
			})
		} else {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, k8sv1.VolumeMount{
				Name:      volumeName,
				MountPath: "/data",
			})
		}
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, k8sv1.VolumeMount{
			Name:      "cacerts",
			ReadOnly:  true,
			MountPath: "/cacerts",
		})
		return tests.RunPod(pod)
	}

	createSourcePodChecker := func(pvc *k8sv1.PersistentVolumeClaim) *k8sv1.Pod {
		volumeName := pvc.GetName()
		podName := "download-pod"
		pod := tests.RenderPod(podName, []string{"/bin/sh", "-c", "sleep 360"}, []string{})
		pod.Spec.Volumes = append(pod.Spec.Volumes, k8sv1.Volume{
			Name: volumeName,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.GetName(),
				},
			},
		})

		volumeMode := pvc.Spec.VolumeMode
		if volumeMode != nil && *volumeMode == k8sv1.PersistentVolumeBlock {
			pod.Spec.Containers[0].VolumeDevices = append(pod.Spec.Containers[0].VolumeDevices, k8sv1.VolumeDevice{
				Name:       volumeName,
				DevicePath: "/dev/volume",
			})
		} else {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, k8sv1.VolumeMount{
				Name:      volumeName,
				MountPath: "/data",
			})
		}
		return tests.RunPod(pod)
	}

	createTriggerPodForPvc := func(pvc *k8sv1.PersistentVolumeClaim) *k8sv1.Pod {
		volumeName := pvc.GetName()
		podName := fmt.Sprintf("bind-%s", volumeName)
		pod := tests.RenderPod(podName, []string{"/bin/sh", "-c", "sleep 1"}, []string{})
		pod.Spec.Volumes = append(pod.Spec.Volumes, k8sv1.Volume{
			Name: volumeName,
			VolumeSource: k8sv1.VolumeSource{
				PersistentVolumeClaim: &k8sv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.GetName(),
				},
			},
		})

		volumeMode := pvc.Spec.VolumeMode
		if volumeMode != nil && *volumeMode == k8sv1.PersistentVolumeBlock {
			pod.Spec.Containers[0].VolumeDevices = append(pod.Spec.Containers[0].VolumeDevices, k8sv1.VolumeDevice{
				Name:       volumeName,
				DevicePath: "/dev/volume",
			})
		} else {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, k8sv1.VolumeMount{
				Name:      volumeName,
				MountPath: "/data",
			})
		}
		return tests.RunPod(pod)
	}

	isWaitForFirstConsumer := func(storageClassName string) bool {
		sc, err := virtClient.StorageV1().StorageClasses().Get(context.Background(), storageClassName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		return sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer
	}

	ensurePVCBound := func(pvc *k8sv1.PersistentVolumeClaim) {
		namespace := pvc.Namespace
		if !isWaitForFirstConsumer(*pvc.Spec.StorageClassName) {
			// Not WFFC, pvc will be bound
			Eventually(func() k8sv1.PersistentVolumeClaimPhase {
				pvc, err := virtClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				return pvc.Status.Phase
			}, 15*time.Second, 1*time.Second).Should(Equal(k8sv1.ClaimBound))
			return
		}
		Eventually(func() k8sv1.PersistentVolumeClaimPhase {
			pvc, err := virtClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			return pvc.Status.Phase
		}, 15*time.Second, 1*time.Second).Should(Equal(k8sv1.ClaimPending))

		triggerPod := createTriggerPodForPvc(pvc)
		Eventually(func() k8sv1.PersistentVolumeClaimPhase {
			pvc, err := virtClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			return pvc.Status.Phase
		}, 30*time.Second, 1*time.Second).Should(Equal(k8sv1.ClaimBound))
		// Delete trigger pod
		immediate := int64(0)
		virtClient.CoreV1().Pods(triggerPod.Namespace).Delete(context.Background(), triggerPod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &immediate,
		})
	}

	createExportTokenSecret := func(pvc *k8sv1.PersistentVolumeClaim) *k8sv1.Secret {
		var err error
		secret := &k8sv1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: pvc.Namespace,
				Name:      fmt.Sprintf("export-token-%s", pvc.Name),
			},
			StringData: map[string]string{
				"token": pvc.Name,
			},
		}
		token, err = virtClient.CoreV1().Secrets(pvc.Namespace).Create(context.Background(), secret, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		return token
	}

	createCaConfigMap := func(name, namespace, data string) *k8sv1.ConfigMap {
		decodedData, err := base64.StdEncoding.DecodeString(data)
		Expect(err).ToNot(HaveOccurred())

		dst := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data: map[string]string{
				caBundleKey: string(decodedData),
			},
		}

		err = virtClient.CoreV1().ConfigMaps(dst.Namespace).Delete(context.TODO(), dst.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			Expect(err).ToNot(HaveOccurred())
		}

		cm, err := virtClient.CoreV1().ConfigMaps(dst.Namespace).Create(context.TODO(), dst, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		return cm
	}

	populateKubeVirtContent := func(sc string) (*k8sv1.PersistentVolumeClaim, string) {
		By("Creating source volume")
		dv := tests.NewRandomDataVolumeWithRegistryImportInStorageClass(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskCirros), util.NamespaceTestDefault, sc, k8sv1.ReadWriteOnce, k8sv1.PersistentVolumeFilesystem)
		_, err = virtClient.CdiClient().CdiV1beta1().DataVolumes(dv.Namespace).Create(context.Background(), dv, metav1.CreateOptions{})
		var pvc *k8sv1.PersistentVolumeClaim
		Eventually(func() *k8sv1.PersistentVolumeClaim {
			pvc, _ = virtClient.CoreV1().PersistentVolumeClaims(dv.Namespace).Get(context.Background(), dv.Name, metav1.GetOptions{})
			return pvc
		}, 15*time.Second, 1*time.Second).ShouldNot(BeNil())
		pvc, err = virtClient.CoreV1().PersistentVolumeClaims(dv.Namespace).Get(context.Background(), dv.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		ensurePVCBound(pvc)

		By("Making sure the DV is successful")
		Eventually(func() cdiv1.DataVolumePhase {
			dv, _ = virtClient.CdiClient().CdiV1beta1().DataVolumes(dv.Namespace).Get(context.Background(), dv.Name, metav1.GetOptions{})
			return dv.Status.Phase
		}, 90*time.Second, 1*time.Second).Should(Equal(cdiv1.Succeeded))

		pod := createSourcePodChecker(pvc)
		command := []string{
			"md5sum",
			filepath.Join(dataPath, "disk.img"),
		}
		out, stderr, err := tests.ExecuteCommandOnPodV2(virtClient, pod, pod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)
		md5sum := strings.Split(out, " ")[0]

		immediate := int64(0)
		err = virtClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &immediate,
		})
		Expect(err).ToNot(HaveOccurred())
		return pvc, md5sum
	}

	populateArchiveContent := func(sc string) (*k8sv1.PersistentVolumeClaim, string) {
		pvc, md5sum := populateKubeVirtContent(sc)
		pvc, err := virtClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())

		pvc.Annotations[annContentType] = "archive"
		pvc, err = virtClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())
		return pvc, md5sum
	}

	verifyKubeVirtRawContent := func(fileName, comparison string, downloadPod *k8sv1.Pod) {
		command := []string{
			"md5sum",
			filepath.Join(dataPath, fileName),
		}
		out, stderr, err := tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)
		md5sum := strings.Split(out, " ")[0]
		Expect(md5sum).To(Equal(comparison))
	}

	verifyKubeVirtGzContent := func(fileName, comparison string, downloadPod *k8sv1.Pod) {
		command := []string{
			"/usr/bin/gzip",
			"-d",
			filepath.Join(dataPath, fileName),
		}
		out, stderr, err := tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)

		fileName = strings.Replace(fileName, ".gz", "", 1)
		command = []string{
			"md5sum",
			filepath.Join(dataPath, fileName),
		}
		out, stderr, err = tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)
		md5sum := strings.Split(out, " ")[0]
		Expect(md5sum).To(Equal(comparison))
	}

	verifyArchiveGzContent := func(fileName, comparison string, downloadPod *k8sv1.Pod) {
		command := []string{
			"/usr/bin/tar",
			"-xzvf",
			filepath.Join(dataPath, fileName),
			"-C",
			dataPath,
		}
		out, stderr, err := tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)

		fileName = "disk.img"
		command = []string{
			"md5sum",
			filepath.Join(dataPath, fileName),
		}
		out, stderr, err = tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)
		md5sum := strings.Split(out, " ")[0]
		Expect(md5sum).To(Equal(comparison))
	}

	DescribeTable("should make a PVC export available", func(populateFunction func(string) (*k8sv1.PersistentVolumeClaim, string),
		verifyFunction func(string, string, *k8sv1.Pod), expectedFormat exportv1.ExportVolumeFormat, urlTemplate string) {
		sc, exists := tests.GetRWOFileSystemStorageClass()
		if !exists {
			Skip("Skip test when Filesystem storage is not present")
		}
		pvc, comparison := populateFunction(sc)
		By("Creating the export token, we can export volumes using this token")
		// For testing the token is the name of the source pvc.
		token := createExportTokenSecret(pvc)

		apiGroup := "v1"
		vmExport := &exportv1.VirtualMachineExport{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-export-%s", pvc.Name),
				Namespace: pvc.Namespace,
			},
			Spec: exportv1.VirtualMachineExportSpec{
				TokenSecretRef: token.Name,
				Source: k8sv1.TypedLocalObjectReference{
					APIGroup: &apiGroup,
					Kind:     "PersistentVolumeClaim",
					Name:     pvc.Name,
				},
			},
		}
		By("Creating VMExport we can start exporting the volume")
		export, err := virtClient.VirtualMachineExport(pvc.Namespace).Create(context.Background(), vmExport, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		Eventually(func() bool {
			export, err = virtClient.VirtualMachineExport(pvc.Namespace).Get(context.Background(), export.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			condReady := false
			if export.Status != nil {
				for _, cond := range export.Status.Conditions {
					if cond.Type == exportv1.ConditionReady && cond.Status == k8sv1.ConditionTrue {
						condReady = true
					}
				}
			}
			return condReady
		}, 30*time.Second, 1*time.Second).Should(BeTrue())

		By("Creating download pod, so we can download image")
		targetPvc := &k8sv1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("target-pvc-%s", pvc.Name),
				Namespace: pvc.Namespace,
			},
			Spec: k8sv1.PersistentVolumeClaimSpec{
				AccessModes:      pvc.Spec.AccessModes,
				StorageClassName: pvc.Spec.StorageClassName,
				Resources:        pvc.Spec.Resources,
			},
		}
		By("Creating target PVC, so we can inspect if the export worked")
		targetPvc, err = virtClient.CoreV1().PersistentVolumeClaims(targetPvc.Namespace).Create(context.Background(), targetPvc, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		caConfigMap := createCaConfigMap("export-cacerts", targetPvc.Namespace, export.Status.Links.Internal.Cert)
		downloadPod := createDownloadPodForPvc(targetPvc, caConfigMap)
		downloadUrl := ""
		fileName := ""
		for _, volume := range export.Status.Links.Internal.Volumes {
			if volume.Name == pvc.Name {
				for _, format := range volume.Formats {
					if format.Format == expectedFormat {
						downloadUrl = fmt.Sprintf(urlTemplate, format.Url, pvc.Name)
						fileName = filepath.Base(format.Url)
					}
				}
			}
		}
		Expect(downloadUrl).ToNot(BeEmpty())
		Expect(fileName).ToNot(BeEmpty())
		command := []string{
			"curl",
			"-L",
			"--cacert",
			filepath.Join(caCertPath, caBundleKey),
			downloadUrl,
			"--output",
			filepath.Join(dataPath, fileName),
		}
		By(fmt.Sprintf("Downloading from URL: %s", downloadUrl))
		out, stderr, err := tests.ExecuteCommandOnPodV2(virtClient, downloadPod, downloadPod.Spec.Containers[0].Name, command)
		Expect(err).ToNot(HaveOccurred(), out, stderr)

		verifyFunction(fileName, comparison, downloadPod)
	},
		Entry("with RAW kubevirt content type", populateKubeVirtContent, verifyKubeVirtRawContent, exportv1.KubeVirtRaw, kubevirtcontentUrlTemplate),
		Entry("with RAW gzipped kubevirt content type", populateKubeVirtContent, verifyKubeVirtGzContent, exportv1.KubeVirtGz, kubevirtcontentUrlTemplate),
		Entry("with archive content type", populateArchiveContent, verifyKubeVirtRawContent, exportv1.Archive, archiveDircontentUrlTemplate),
		Entry("with archive tarred gzipped content type", populateArchiveContent, verifyArchiveGzContent, exportv1.ArchiveGz, kubevirtcontentUrlTemplate),
	)
})
