// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package backup

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/backup/testutils"
	"github.com/pingcap/tidb-operator/pkg/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

type helper struct {
	testutils.Helper
}

func newHelper(t *testing.T) *helper {
	h := testutils.NewHelper(t)
	return &helper{*h}
}

// TODO: refactor to reduce duplicated code with restore tests
func (h *helper) hasCondition(ns string, name string, tp v1alpha1.BackupConditionType, reasonSub string) {
	h.T.Helper()
	g := NewGomegaWithT(h.T)
	get, err := h.Deps.Clientset.PingcapV1alpha1().Backups(ns).Get(context.TODO(), name, metav1.GetOptions{})
	g.Expect(err).Should(BeNil())
	for _, c := range get.Status.Conditions {
		if c.Type == tp {
			if reasonSub == "" || strings.Contains(c.Reason, reasonSub) {
				return
			}
			h.T.Fatalf("%s do not match reason %s", reasonSub, c.Reason)
		}
	}
	h.T.Fatalf("%s/%s do not has condition type: %s, cur conds: %v", ns, name, tp, get.Status.Conditions)
}

func invalidBackup() *v1alpha1.Backup {
	b := &v1alpha1.Backup{}
	b.Namespace = "ns"
	b.Name = "invalid_name"
	return b
}

func validDumplingBackup() *v1alpha1.Backup {
	b := &v1alpha1.Backup{
		Spec: v1alpha1.BackupSpec{
			From: &v1alpha1.TiDBAccessConfig{
				Host:                "localhost",
				SecretName:          "secretName",
				TLSClientSecretName: pointer.StringPtr("secretName"),
			},
			StorageSize: "1G",
			StorageProvider: v1alpha1.StorageProvider{
				S3: &v1alpha1.S3StorageProvider{
					Bucket: "s3",
					Prefix: "prefix-",
				},
			},
			Env: []corev1.EnvVar{
				{
					Name:  "env_name",
					Value: "env_value",
				},
				// existing env name will be overwritten for backup
				{
					Name:  "S3_PROVIDER",
					Value: "fake_provider",
				},
			},
		},
	}

	b.Namespace = "ns"
	b.Name = "dump_name"

	return b
}

func genValidBRBackups() []*v1alpha1.Backup {
	var bs []*v1alpha1.Backup

	for i, sp := range testutils.GenValidStorageProviders() {
		b := &v1alpha1.Backup{
			Spec: v1alpha1.BackupSpec{
				From: &v1alpha1.TiDBAccessConfig{
					Host:       "localhost",
					SecretName: fmt.Sprintf("backup_secret_%d", i),
				},
				StorageSize:     "1G",
				StorageProvider: sp,
				Type:            v1alpha1.BackupTypeDB,
				BR: &v1alpha1.BRConfig{
					ClusterNamespace: "ns",
					Cluster:          fmt.Sprintf("tidb_%d", i),
					DB:               "dbName",
				},
				Env: []corev1.EnvVar{
					{
						Name:  fmt.Sprintf("env_name_%d", i),
						Value: fmt.Sprintf("env_value_%d", i),
					},
					// existing env name will be overwritten for backup
					{
						Name:  "BR_LOG_TO_TERM",
						Value: "value",
					},
					// existing env name will be overwritten for cleaner
					{
						Name:  "S3_PROVIDER",
						Value: "value",
					},
				},
			},
		}
		b.Namespace = "ns"
		b.Name = fmt.Sprintf("backup_name_%d", i)
		bs = append(bs, b)
	}

	return bs
}

func TestBackupManagerDumpling(t *testing.T) {
	g := NewGomegaWithT(t)

	helper := newHelper(t)
	defer helper.Close()
	deps := helper.Deps
	var err error

	bm := NewBackupManager(deps).(*backupManager)

	// create backup
	backup := validDumplingBackup()
	_, err = deps.Clientset.PingcapV1alpha1().Backups(backup.Namespace).Create(context.TODO(), backup, metav1.CreateOptions{})
	g.Expect(err).Should(BeNil())

	// create relate secret
	helper.CreateSecret(backup)

	err = bm.syncBackupJob(backup)
	g.Expect(err).Should(BeNil())
	helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupScheduled, "")
	job, err := deps.KubeClientset.BatchV1().Jobs(backup.Namespace).Get(context.TODO(), backup.GetBackupJobName(), metav1.GetOptions{})
	g.Expect(err).Should(BeNil())

	// check pod env are set correctly
	env1 := corev1.EnvVar{
		Name:  "env_name",
		Value: "env_value",
	}
	env2Yes := corev1.EnvVar{
		Name:  "S3_PROVIDER",
		Value: "fake_provider",
	}
	env2No := corev1.EnvVar{
		Name:  "S3_PROVIDER",
		Value: "",
	}
	g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(gomega.ContainElement(env1))
	g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(gomega.ContainElement(env2Yes))
	g.Expect(job.Spec.Template.Spec.Containers[0].Env).NotTo(gomega.ContainElement(env2No))
}

func TestBackupManagerBR(t *testing.T) {
	g := NewGomegaWithT(t)
	helper := newHelper(t)
	defer helper.Close()
	deps := helper.Deps
	var err error

	bm := NewBackupManager(deps).(*backupManager)

	// test invalid Backup spec
	backup := invalidBackup()
	_, err = deps.Clientset.PingcapV1alpha1().Backups(backup.Namespace).Create(context.TODO(), backup, metav1.CreateOptions{})
	g.Expect(err).Should(BeNil())
	err = bm.syncBackupJob(backup)
	g.Expect(err).ShouldNot(BeNil())
	helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupInvalid, "")

	// test valid backups
	for i, backup := range genValidBRBackups() {
		_, err := deps.Clientset.PingcapV1alpha1().Backups(backup.Namespace).Create(context.TODO(), backup, metav1.CreateOptions{})
		g.Expect(err).Should(BeNil())

		// create relate secret
		helper.CreateSecret(backup)

		// failed to get relate tc
		err = bm.syncBackupJob(backup)
		g.Expect(err).ShouldNot(BeNil())
		helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupRetryFailed, "failed to fetch tidbcluster")

		// create relate tc and try again should success and job created.
		helper.CreateTC(backup.Spec.BR.ClusterNamespace, backup.Spec.BR.Cluster)
		err = bm.syncBackupJob(backup)
		g.Expect(err).Should(BeNil())
		helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupScheduled, "")
		job, err := deps.KubeClientset.BatchV1().Jobs(backup.Namespace).Get(context.TODO(), backup.GetBackupJobName(), metav1.GetOptions{})
		g.Expect(err).Should(BeNil())

		// check pod env are set correctly
		env1 := corev1.EnvVar{
			Name:  fmt.Sprintf("env_name_%d", i),
			Value: fmt.Sprintf("env_value_%d", i),
		}
		env2Yes := corev1.EnvVar{
			Name:  "BR_LOG_TO_TERM",
			Value: "value",
		}
		env2No := corev1.EnvVar{
			Name:  "BR_LOG_TO_TERM",
			Value: string(rune(1)),
		}
		g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(gomega.ContainElement(env1))
		g.Expect(job.Spec.Template.Spec.Containers[0].Env).To(gomega.ContainElement(env2Yes))
		g.Expect(job.Spec.Template.Spec.Containers[0].Env).NotTo(gomega.ContainElement(env2No))
	}
}

func TestClean(t *testing.T) {
	g := NewGomegaWithT(t)
	helper := newHelper(t)
	defer helper.Close()
	deps := helper.Deps

	for _, backup := range genValidBRBackups() {
		_, err := deps.Clientset.PingcapV1alpha1().Backups(backup.Namespace).Create(context.TODO(), backup, metav1.CreateOptions{})
		g.Expect(err).Should(BeNil())
		helper.CreateSecret(backup)
		helper.CreateTC(backup.Spec.BR.ClusterNamespace, backup.Spec.BR.Cluster)

		// make the backup need to be clean
		backup.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		backup.Spec.CleanPolicy = v1alpha1.CleanPolicyTypeDelete

		statusUpdater := controller.NewRealBackupConditionUpdater(deps.Clientset, deps.BackupLister, deps.Recorder)
		bc := NewBackupCleaner(deps, statusUpdater)

		// test empty backup.Status.BackupPath
		backup.Status.BackupPath = ""
		err = bc.Clean(backup)
		g.Expect(err).Should(BeNil())
		helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupClean, "")

		// test clean job created
		backup.Status.BackupPath = "/path"
		err = bc.Clean(backup)
		g.Expect(err).Should(BeNil())
		helper.hasCondition(backup.Namespace, backup.Name, v1alpha1.BackupClean, "")
		_, err = deps.KubeClientset.BatchV1().Jobs(backup.Namespace).Get(context.TODO(), backup.GetCleanJobName(), metav1.GetOptions{})
		g.Expect(err).Should(BeNil())
		// test already have a clean job running
		g.Eventually(func() error {
			_, err := deps.JobLister.Jobs(backup.Namespace).Get(backup.GetCleanJobName())
			return err
		}, time.Second*10).Should(BeNil())
		err = bc.Clean(backup)
		g.Expect(err).Should(BeNil())
	}
}
