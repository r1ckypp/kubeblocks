/*
Copyright (C) 2022-2023 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package backup

import (
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/internal/constant"
	intctrlutil "github.com/apecloud/kubeblocks/internal/controllerutil"
	"github.com/apecloud/kubeblocks/internal/dataprotection/action"
	dptypes "github.com/apecloud/kubeblocks/internal/dataprotection/types"
	"github.com/apecloud/kubeblocks/internal/dataprotection/utils"
	"github.com/apecloud/kubeblocks/internal/dataprotection/utils/boolptr"
	viper "github.com/apecloud/kubeblocks/internal/viperx"
)

const (
	BackupDataJobNamePrefix   = "dp-backup"
	prebackupJobNamePrefix    = "dp-prebackup"
	postbackupJobNamePrefix   = "dp-postbackup"
	backupDataContainerName   = "backupdata"
	syncProgressContainerName = "sync-progress"
)

// Request is a request for a backup, with all references to other objects.
type Request struct {
	*dpv1alpha1.Backup
	intctrlutil.RequestCtx

	Client        client.Client
	BackupPolicy  *dpv1alpha1.BackupPolicy
	BackupMethod  *dpv1alpha1.BackupMethod
	ActionSet     *dpv1alpha1.ActionSet
	TargetPods    []*corev1.Pod
	BackupRepoPVC *corev1.PersistentVolumeClaim
	BackupRepo    *dpv1alpha1.BackupRepo
}

func (r *Request) GetBackupType() string {
	if r.ActionSet != nil {
		return string(r.ActionSet.Spec.BackupType)
	}
	if r.BackupMethod != nil && boolptr.IsSetToTrue(r.BackupMethod.SnapshotVolumes) {
		return string(dpv1alpha1.BackupTypeFull)
	}
	return ""
}

// BuildActions builds the actions for the backup.
func (r *Request) BuildActions() ([]action.Action, error) {
	var actions []action.Action

	appendIgnoreNil := func(elems ...action.Action) {
		for _, elem := range elems {
			if elem == nil || reflect.ValueOf(elem).IsNil() {
				continue
			}
			actions = append(actions, elem)
		}
	}

	// build pre-backup actions
	preBackupActions, err := r.buildPreBackupActions()
	if err != nil {
		return nil, err
	}

	// build backup data action
	backupDataAction, err := r.buildBackupDataAction()
	if err != nil {
		return nil, err
	}

	// build create volume snapshot action
	createVolumeSnapshotAction, err := r.buildCreateVolumeSnapshotAction()
	if err != nil {
		return nil, err
	}

	// build backup kubernetes resources action
	backupKubeResourcesAction, err := r.buildBackupKubeResourcesAction()
	if err != nil {
		return nil, err
	}

	// build post-backup actions
	postBackupActions, err := r.buildPostBackupActions()
	if err != nil {
		return nil, err
	}

	appendIgnoreNil(preBackupActions...)
	appendIgnoreNil(backupDataAction, createVolumeSnapshotAction, backupKubeResourcesAction)
	appendIgnoreNil(postBackupActions...)
	return actions, nil
}

func (r *Request) buildPreBackupActions() ([]action.Action, error) {
	if !r.backupActionSetExists() ||
		len(r.ActionSet.Spec.Backup.PreBackup) == 0 {
		return nil, nil
	}

	var actions []action.Action
	for i, preBackup := range r.ActionSet.Spec.Backup.PreBackup {
		a, err := r.buildAction(fmt.Sprintf("%s-%d", prebackupJobNamePrefix, i), &preBackup)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func (r *Request) buildPostBackupActions() ([]action.Action, error) {
	if !r.backupActionSetExists() ||
		len(r.ActionSet.Spec.Backup.PostBackup) == 0 {
		return nil, nil
	}

	var actions []action.Action
	for i, postBackup := range r.ActionSet.Spec.Backup.PostBackup {
		a, err := r.buildAction(fmt.Sprintf("%s-%d", postbackupJobNamePrefix, i), &postBackup)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, nil
}

func (r *Request) buildBackupDataAction() (action.Action, error) {
	if !r.backupActionSetExists() ||
		r.ActionSet.Spec.Backup.BackupData == nil {
		return nil, nil
	}

	backupDataAct := r.ActionSet.Spec.Backup.BackupData
	podSpec := r.buildJobActionPodSpec(backupDataContainerName, &backupDataAct.JobActionSpec)
	if backupDataAct.SyncProgress != nil {
		r.injectSyncProgressContainer(podSpec, backupDataAct.SyncProgress)
	}

	if r.ActionSet.Spec.BackupType == dpv1alpha1.BackupTypeFull {
		return &action.JobAction{
			Name:         BackupDataJobNamePrefix,
			ObjectMeta:   *buildBackupJobObjMeta(r.Backup, BackupDataJobNamePrefix),
			Owner:        r.Backup,
			PodSpec:      podSpec,
			BackOffLimit: r.BackupPolicy.Spec.BackoffLimit,
		}, nil
	}
	return nil, fmt.Errorf("unsupported backup type %s", r.ActionSet.Spec.BackupType)
}

func (r *Request) buildCreateVolumeSnapshotAction() (action.Action, error) {
	targetPod := r.TargetPods[0]
	if r.BackupMethod == nil ||
		!boolptr.IsSetToTrue(r.BackupMethod.SnapshotVolumes) {
		return nil, nil
	}

	if r.BackupMethod.TargetVolumes == nil {
		return nil, fmt.Errorf("targetVolumes is required for snapshotVolumes")
	}

	if !utils.VolumeSnapshotEnabled() {
		return nil, fmt.Errorf("volume snapshot is not enabled")
	}

	pvcs, err := getPVCsByVolumeNames(r.Client, targetPod, r.BackupMethod.TargetVolumes.Volumes)
	if err != nil {
		return nil, err
	}

	if len(pvcs) == 0 {
		return nil, fmt.Errorf("no PVCs found for pod %s to back up", targetPod.Name)
	}

	return &action.CreateVolumeSnapshotAction{
		Name: "createVolumeSnapshot",
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.Backup.Namespace,
			Name:      r.Backup.Name,
			Labels:    BuildBackupWorkloadLabels(r.Backup),
		},
		Owner:                         r.Backup,
		PersistentVolumeClaimWrappers: pvcs,
	}, nil
}

// TODO(ldm): implement this
func (r *Request) buildBackupKubeResourcesAction() (action.Action, error) {
	return nil, nil
}

func (r *Request) buildAction(name string, act *dpv1alpha1.ActionSpec) (action.Action, error) {
	if act.Exec == nil && act.Job == nil {
		return nil, fmt.Errorf("action %s has no exec or job", name)
	}
	if act.Exec != nil && act.Job != nil {
		return nil, fmt.Errorf("action %s should have only one of exec or job", name)
	}
	switch {
	case act.Exec != nil:
		return r.buildExecAction(name, act.Exec), nil
	case act.Job != nil:
		return r.buildJobAction(name, act.Job)
	}
	return nil, nil
}

func (r *Request) buildExecAction(name string, exec *dpv1alpha1.ExecActionSpec) action.Action {
	targetPod := r.TargetPods[0]
	return &action.ExecAction{
		JobAction: action.JobAction{
			Name:       name,
			ObjectMeta: *buildBackupJobObjMeta(r.Backup, name),
			Owner:      r.Backup,
		},
		Command:            exec.Command,
		Container:          exec.Container,
		Namespace:          targetPod.Namespace,
		PodName:            targetPod.Name,
		Timeout:            exec.Timeout,
		ServiceAccountName: r.targetServiceAccountName(),
	}
}

func (r *Request) buildJobAction(name string, job *dpv1alpha1.JobActionSpec) (action.Action, error) {
	return &action.JobAction{
		Name:         name,
		ObjectMeta:   *buildBackupJobObjMeta(r.Backup, name),
		Owner:        r.Backup,
		PodSpec:      r.buildJobActionPodSpec(name, job),
		BackOffLimit: r.BackupPolicy.Spec.BackoffLimit,
	}, nil
}

func (r *Request) buildJobActionPodSpec(name string, job *dpv1alpha1.JobActionSpec) *corev1.PodSpec {
	targetPod := r.TargetPods[0]
	// build environment variables, include built-in envs, envs from backupMethod
	// and envs from actionSet. Latter will override former for the same name.
	// env from backupMethod has the highest priority.
	buildEnv := func() []corev1.EnvVar {
		envVars := []corev1.EnvVar{
			{
				Name:  dptypes.DPBackupName,
				Value: r.Backup.Name,
			},
			{
				Name:  dptypes.DPBackupDIR,
				Value: buildBackupPathInContainer(r.Backup, r.BackupPolicy.Spec.PathPrefix),
			},
			{
				Name:  dptypes.DPTargetPodName,
				Value: targetPod.Name,
			},
			{
				Name:  dptypes.DPTTL,
				Value: r.Spec.RetentionPeriod.String(),
			},
		}
		envVars = append(envVars, utils.BuildEnvByCredential(targetPod, r.BackupPolicy.Spec.Target.ConnectionCredential)...)
		if r.ActionSet != nil {
			envVars = append(envVars, r.ActionSet.Spec.Env...)
		}
		return utils.MergeEnv(envVars, r.BackupMethod.Env)
	}

	buildVolumes := func() []corev1.Volume {
		return append([]corev1.Volume{
			buildBackupRepoVolume(r.BackupRepoPVC.Name),
		}, getVolumesByVolumeInfo(targetPod, r.BackupMethod.TargetVolumes)...)
	}

	buildVolumeMounts := func() []corev1.VolumeMount {
		return append([]corev1.VolumeMount{
			buildBackupRepoVolumeMount(r.BackupRepoPVC.Name),
		}, getVolumeMountsByVolumeInfo(targetPod, r.BackupMethod.TargetVolumes)...)
	}

	runAsUser := int64(0)
	container := corev1.Container{
		Name:            name,
		Image:           job.Image,
		Command:         job.Command,
		Env:             buildEnv(),
		VolumeMounts:    buildVolumeMounts(),
		ImagePullPolicy: corev1.PullPolicy(viper.GetString(constant.KBImagePullPolicy)),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolptr.False(),
			RunAsUser:                &runAsUser,
		},
	}

	if r.BackupMethod.RuntimeSettings != nil {
		container.Resources = r.BackupMethod.RuntimeSettings.Resources
	}

	if r.ActionSet != nil {
		container.EnvFrom = r.ActionSet.Spec.EnvFrom
	}

	intctrlutil.InjectZeroResourcesLimitsIfEmpty(&container)

	podSpec := &corev1.PodSpec{
		Containers:         []corev1.Container{container},
		Volumes:            buildVolumes(),
		ServiceAccountName: r.targetServiceAccountName(),
		RestartPolicy:      corev1.RestartPolicyNever,

		// tolerate all taints
		Tolerations: []corev1.Toleration{
			{
				Operator: corev1.TolerationOpExists,
			},
		},
	}

	if boolptr.IsSetToTrue(job.RunOnTargetPodNode) {
		podSpec.NodeSelector = map[string]string{
			corev1.LabelHostname: targetPod.Spec.NodeName,
		}
	}
	return podSpec
}

// injectSyncProgressContainer injects a container to sync the backup progress.
func (r *Request) injectSyncProgressContainer(podSpec *corev1.PodSpec,
	sync *dpv1alpha1.SyncProgress) {
	if !boolptr.IsSetToTrue(sync.Enabled) {
		return
	}

	// build container to sync backup progress that will update the backup status
	container := podSpec.Containers[0].DeepCopy()
	container.Name = syncProgressContainerName
	container.Image = viper.GetString(constant.KBToolsImage)
	container.ImagePullPolicy = corev1.PullPolicy(viper.GetString(constant.KBImagePullPolicy))
	container.Resources = corev1.ResourceRequirements{Limits: nil, Requests: nil}
	intctrlutil.InjectZeroResourcesLimitsIfEmpty(container)
	container.Command = []string{"sh", "-c"}

	// append some envs
	checkIntervalSeconds := int32(5)
	if sync.IntervalSeconds != nil && *sync.IntervalSeconds > 0 {
		checkIntervalSeconds = *sync.IntervalSeconds
	}
	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  dptypes.DPBackupInfoFile,
			Value: buildBackupInfoFilePath(r.Backup, r.BackupPolicy.Spec.PathPrefix),
		},
		corev1.EnvVar{
			Name:  dptypes.DPCheckInterval,
			Value: fmt.Sprintf("%d", checkIntervalSeconds)},
	)

	args := fmt.Sprintf(`
set -o errexit;
set -o nounset;
while [ ! -f ${%[1]s} ]; do
  sleep ${%[2]s}
done
backupInfo=$(cat ${%[1]s});
echo backupInfo:${backupInfo};
eval kubectl -n %[3]s patch backup %[4]s --subresource=status --type=merge --patch '{\"status\":${backupInfo}}';
`, dptypes.DPBackupInfoFile, dptypes.DPCheckInterval, r.Backup.Namespace, r.Backup.Name)

	container.Args = []string{args}
	podSpec.Containers = append(podSpec.Containers, *container)
}

func (r *Request) backupActionSetExists() bool {
	return r.ActionSet != nil && r.ActionSet.Spec.Backup != nil
}

func (r *Request) targetServiceAccountName() string {
	saName := r.BackupPolicy.Spec.Target.ServiceAccountName
	if len(saName) > 0 {
		return saName
	}
	// service account name is not specified, use the target pod service account
	targetPod := r.TargetPods[0]
	return targetPod.Spec.ServiceAccountName
}
