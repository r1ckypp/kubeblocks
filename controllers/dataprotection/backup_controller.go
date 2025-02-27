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

package dataprotection

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	vsv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v3/apis/volumesnapshot/v1beta1"
	vsv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/apecloud/kubeblocks/apis/apps/v1alpha1"
	dpv1alpha1 "github.com/apecloud/kubeblocks/apis/dataprotection/v1alpha1"
	"github.com/apecloud/kubeblocks/internal/constant"
	"github.com/apecloud/kubeblocks/internal/controller/model"
	intctrlutil "github.com/apecloud/kubeblocks/internal/controllerutil"
	"github.com/apecloud/kubeblocks/internal/dataprotection/action"
	dpbackup "github.com/apecloud/kubeblocks/internal/dataprotection/backup"
	dperrors "github.com/apecloud/kubeblocks/internal/dataprotection/errors"
	dptypes "github.com/apecloud/kubeblocks/internal/dataprotection/types"
	dputils "github.com/apecloud/kubeblocks/internal/dataprotection/utils"
	"github.com/apecloud/kubeblocks/internal/dataprotection/utils/boolptr"
	viper "github.com/apecloud/kubeblocks/internal/viperx"
)

// BackupReconciler reconciles a Backup object
type BackupReconciler struct {
	client.Client
	Scheme     *k8sruntime.Scheme
	Recorder   record.EventRecorder
	RestConfig *rest.Config
	clock      clock.RealClock
}

// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dataprotection.kubeblocks.io,resources=backups/finalizers,verbs=update

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots/finalizers,verbs=update;patch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses/finalizers,verbs=update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the backup closer to the desired state.
func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// setup common request context
	reqCtx := intctrlutil.RequestCtx{
		Ctx:      ctx,
		Req:      req,
		Log:      log.FromContext(ctx).WithValues("backup", req.NamespacedName),
		Recorder: r.Recorder,
	}

	// get backup object, and return if not found
	backup := &dpv1alpha1.Backup{}
	if err := r.Client.Get(reqCtx.Ctx, reqCtx.Req.NamespacedName, backup); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}

	reqCtx.Log.V(1).Info("reconcile", "backup", req.NamespacedName, "phase", backup.Status.Phase)

	// if backup is being deleted, set backup phase to Deleting. The backup
	// reference workloads, data and volume snapshots will be deleted by controller
	// later when the backup status.phase is deleting.
	if !backup.GetDeletionTimestamp().IsZero() && backup.Status.Phase != dpv1alpha1.BackupPhaseDeleting {
		patch := client.MergeFrom(backup.DeepCopy())
		backup.Status.Phase = dpv1alpha1.BackupPhaseDeleting
		if err := r.Client.Status().Patch(reqCtx.Ctx, backup, patch); err != nil {
			return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
		}
	}

	switch backup.Status.Phase {
	case "", dpv1alpha1.BackupPhaseNew:
		return r.handleNewPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseRunning:
		return r.handleRunningPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseCompleted:
		return r.handleCompletedPhase(reqCtx, backup)
	case dpv1alpha1.BackupPhaseDeleting:
		return r.handleDeletingPhase(reqCtx, backup)
	default:
		return intctrlutil.Reconciled()
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&dpv1alpha1.Backup{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: viper.GetInt(maxConcurDataProtectionReconKey),
		}).
		Owns(&batchv1.Job{})

	if intctrlutil.InVolumeSnapshotV1Beta1() {
		b.Owns(&vsv1beta1.VolumeSnapshot{}, builder.Predicates{})
	} else {
		b.Owns(&vsv1.VolumeSnapshot{}, builder.Predicates{})
	}
	return b.Complete(r)
}

// deleteBackupFiles deletes the backup files stored in backup repository.
func (r *BackupReconciler) deleteBackupFiles(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	deleteBackup := func() error {
		// remove backup finalizers to delete it
		patch := client.MergeFrom(backup.DeepCopy())
		controllerutil.RemoveFinalizer(backup, dptypes.DataProtectionFinalizerName)
		return r.Patch(reqCtx.Ctx, backup, patch)
	}

	deleter := &dpbackup.Deleter{
		RequestCtx: reqCtx,
		Client:     r.Client,
		Scheme:     r.Scheme,
	}

	status, err := deleter.DeleteBackupFiles(backup)
	switch status {
	case dpbackup.DeletionStatusSucceeded:
		return deleteBackup()
	case dpbackup.DeletionStatusFailed:
		failureReason := err.Error()
		if backup.Status.FailureReason == failureReason {
			return nil
		}
		backupPatch := client.MergeFrom(backup.DeepCopy())
		backup.Status.FailureReason = failureReason
		r.Recorder.Event(backup, corev1.EventTypeWarning, "DeleteBackupFilesFailed", failureReason)
		return r.Status().Patch(reqCtx.Ctx, backup, backupPatch)
	case dpbackup.DeletionStatusDeleting,
		dpbackup.DeletionStatusUnknown:
		// wait for the deletion job completed
		return err
	}
	return err
}

// handleDeletingPhase handles the deletion of backup. It will delete the backup CR
// and the backup workload(job/statefulset).
func (r *BackupReconciler) handleDeletingPhase(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	// if backup phase is Deleting, delete the backup reference workloads,
	// backup data stored in backup repository and volume snapshots.
	// TODO(ldm): if backup is being used by restore, do not delete it.
	if err := r.deleteExternalResources(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}

	if backup.Spec.DeletionPolicy == dpv1alpha1.BackupDeletionPolicyRetain {
		return intctrlutil.Reconciled()
	}

	if err := r.deleteVolumeSnapshots(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}

	if err := r.deleteBackupFiles(reqCtx, backup); err != nil {
		return intctrlutil.RequeueWithError(err, reqCtx.Log, "")
	}
	return intctrlutil.Reconciled()
}

func (r *BackupReconciler) handleNewPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	request, err := r.prepareBackupRequest(reqCtx, backup)
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup.DeepCopy(), backup, err)
	}

	// set and patch backup object meta, including labels, annotations and finalizers
	// if the backup object meta is changed, the backup object will be patched.
	if patched, err := r.patchBackupObjectMeta(backup, request); err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	} else if patched {
		return intctrlutil.Reconciled()
	}

	// set and patch backup status
	if err = r.patchBackupStatus(backup, request); err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	}
	return intctrlutil.Reconciled()
}

// prepareBackupRequest prepares a request for a backup, with all references to
// other kubernetes objects, and validate them.
func (r *BackupReconciler) prepareBackupRequest(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (*dpbackup.Request, error) {
	request := &dpbackup.Request{
		Backup:     backup.DeepCopy(),
		RequestCtx: reqCtx,
		Client:     r.Client,
	}

	if request.Annotations == nil {
		request.Annotations = make(map[string]string)
	}

	if request.Labels == nil {
		request.Labels = make(map[string]string)
	}

	backupPolicy, err := getBackupPolicyByName(reqCtx, r.Client, backup.Spec.BackupPolicyName)
	if err != nil {
		return nil, err
	}

	targetPods, err := getTargetPods(reqCtx, r.Client,
		backup.Annotations[dataProtectionBackupTargetPodKey], backupPolicy)
	if err != nil || len(targetPods) == 0 {
		return nil, fmt.Errorf("failed to get target pods by backup policy %s/%s",
			backupPolicy.Namespace, backupPolicy.Name)
	}

	if len(targetPods) > 1 {
		return nil, fmt.Errorf("do not support more than one target pods")
	}

	backupMethod := getBackupMethodByName(backup.Spec.BackupMethod, backupPolicy)
	if backupMethod == nil {
		return nil, intctrlutil.NewNotFound("backupMethod: %s not found",
			backup.Spec.BackupMethod)
	}

	// backupMethod should specify snapshotVolumes or actionSetName, if we take
	// snapshots to back up volumes, the snapshotVolumes should be set to true
	// and the actionSetName is not required, if we do not take snapshots to back
	// up volumes, the actionSetName is required.
	snapshotVolumes := boolptr.IsSetToTrue(backupMethod.SnapshotVolumes)
	if !snapshotVolumes && backupMethod.ActionSetName == "" {
		return nil, fmt.Errorf("backup method %s should specify snapshotVolumes or actionSetName", backupMethod.Name)
	}

	// if backup method use volume snapshots to back up, the volume snapshot
	// feature should be enabled.
	if snapshotVolumes && !dputils.VolumeSnapshotEnabled() {
		return nil, fmt.Errorf("current backup method depends on volume snapshot, but volume snapshot is not enabled")
	}

	if backupMethod.ActionSetName != "" {
		actionSet, err := getActionSetByName(reqCtx, r.Client, backupMethod.ActionSetName)
		if err != nil {
			return nil, err
		}
		if actionSet.Spec.BackupType != dpv1alpha1.BackupTypeFull {
			return nil, fmt.Errorf("only support backup type Full for actionSet %s", actionSet.Name)
		}
		request.ActionSet = actionSet
	}

	request.BackupPolicy = backupPolicy
	if err = r.handleBackupRepo(request); err != nil {
		return nil, err
	}

	request.BackupMethod = backupMethod
	request.TargetPods = targetPods
	return request, nil
}

// handleBackupRepo handles the backup repo, and get the backup repo PVC. If the
// PVC is not present, it will add a special label and wait for the backup repo
// controller to create the PVC.
func (r *BackupReconciler) handleBackupRepo(request *dpbackup.Request) error {
	repo, err := r.getBackupRepo(request.Ctx, request.Backup, request.BackupPolicy)
	if err != nil {
		return err
	}
	request.BackupRepo = repo

	pvcName := repo.Status.BackupPVCName
	if pvcName == "" {
		return dperrors.NewBackupPVCNameIsEmpty(repo.Name, request.Spec.BackupPolicyName)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvcKey := client.ObjectKey{Namespace: request.Req.Namespace, Name: pvcName}
	if err = r.Client.Get(request.Ctx, pvcKey, pvc); err != nil {
		return client.IgnoreNotFound(err)
	}

	// backupRepo PVC exists, record the PVC name
	if err == nil {
		request.BackupRepoPVC = pvc
	}
	return nil
}

func (r *BackupReconciler) patchBackupStatus(
	original *dpv1alpha1.Backup,
	request *dpbackup.Request) error {
	request.Status.FormatVersion = dpbackup.FormatVersion
	request.Status.Path = dpbackup.BuildBackupPath(request.Backup, request.BackupPolicy.Spec.PathPrefix)
	request.Status.Target = request.BackupPolicy.Spec.Target
	request.Status.BackupMethod = request.BackupMethod
	request.Status.PersistentVolumeClaimName = request.BackupRepoPVC.Name
	request.Status.BackupRepoName = request.BackupRepo.Name

	// init action status
	actions, err := request.BuildActions()
	if err != nil {
		return err
	}
	request.Status.Actions = make([]dpv1alpha1.ActionStatus, len(actions))
	for i, act := range actions {
		request.Status.Actions[i] = dpv1alpha1.ActionStatus{
			Name:       act.GetName(),
			Phase:      dpv1alpha1.ActionPhaseNew,
			ActionType: act.Type(),
		}
	}

	// update phase to running
	request.Status.Phase = dpv1alpha1.BackupPhaseRunning
	request.Status.StartTimestamp = &metav1.Time{Time: r.clock.Now().UTC()}

	duration, err := original.Spec.RetentionPeriod.ToDuration()
	if err != nil {
		return fmt.Errorf("failed to parse retention period %s, %v", original.Spec.RetentionPeriod, err)
	}
	if original.Spec.RetentionPeriod != "" {
		request.Status.Expiration = &metav1.Time{
			Time: request.Status.StartTimestamp.Add(duration),
		}
	}
	return r.Client.Status().Patch(request.Ctx, request.Backup, client.MergeFrom(original))
}

// patchBackupObjectMeta patches backup object metaObject include cluster snapshot.
func (r *BackupReconciler) patchBackupObjectMeta(
	original *dpv1alpha1.Backup,
	request *dpbackup.Request) (bool, error) {
	targetPod := request.TargetPods[0]

	// get KubeBlocks cluster and set labels and annotations for backup
	// TODO(ldm): we should remove this dependency of cluster in the future
	cluster := getCluster(request.Ctx, r.Client, targetPod)
	if cluster != nil {
		if err := setClusterSnapshotAnnotation(request.Backup, cluster); err != nil {
			return false, err
		}
		request.Labels[dptypes.DataProtectionLabelClusterUIDKey] = string(cluster.UID)
	}
	for _, v := range getClusterLabelKeys() {
		request.Labels[v] = targetPod.Labels[v]
	}

	request.Labels[dataProtectionBackupRepoKey] = request.BackupRepo.Name
	request.Labels[constant.AppManagedByLabelKey] = constant.AppName
	request.Labels[dataProtectionLabelBackupTypeKey] = request.GetBackupType()

	// if the backupRepo PVC is not present, add a special label and wait for the
	// backup repo controller to create the PVC.
	wait := false
	if request.BackupRepoPVC == nil {
		request.Labels[dataProtectionWaitRepoPreparationKey] = trueVal
		wait = true
	}

	// set annotations
	request.Annotations[dataProtectionBackupTargetPodKey] = targetPod.Name

	// set finalizer
	controllerutil.AddFinalizer(request.Backup, dptypes.DataProtectionFinalizerName)

	if reflect.DeepEqual(original.ObjectMeta, request.ObjectMeta) {
		return wait, nil
	}

	return true, r.Client.Patch(request.Ctx, request.Backup, client.MergeFrom(original))
}

// getBackupRepo returns the backup repo specified by the backup object or the policy.
// if no backup repo specified, it will return the default one.
func (r *BackupReconciler) getBackupRepo(ctx context.Context,
	backup *dpv1alpha1.Backup,
	backupPolicy *dpv1alpha1.BackupPolicy) (*dpv1alpha1.BackupRepo, error) {
	// use the specified backup repo
	var repoName string
	if val := backup.Labels[dataProtectionBackupRepoKey]; val != "" {
		repoName = val
	} else if backupPolicy.Spec.BackupRepoName != nil && *backupPolicy.Spec.BackupRepoName != "" {
		repoName = *backupPolicy.Spec.BackupRepoName
	}
	if repoName != "" {
		repo := &dpv1alpha1.BackupRepo{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: repoName}, repo); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, intctrlutil.NewNotFound("backup repo %s not found", repoName)
			}
			return nil, err
		}
		return repo, nil
	}
	// fallback to use the default repo
	return getDefaultBackupRepo(ctx, r.Client)
}

func (r *BackupReconciler) handleRunningPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	request, err := r.prepareBackupRequest(reqCtx, backup)
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup.DeepCopy(), backup, err)
	}

	// there are actions not completed, continue to handle following actions
	actions, err := request.BuildActions()
	if err != nil {
		return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
	}

	actionCtx := action.Context{
		Ctx:              reqCtx.Ctx,
		Client:           r.Client,
		Recorder:         r.Recorder,
		Scheme:           r.Scheme,
		RestClientConfig: r.RestConfig,
	}

	// check all actions status, if any action failed, update backup status to failed
	// if all actions completed, update backup status to completed, otherwise,
	// continue to handle following actions.
	for i, act := range actions {
		status, err := act.Execute(actionCtx)
		if err != nil {
			return r.updateStatusIfFailed(reqCtx, backup, request.Backup, err)
		}
		request.Status.Actions[i] = mergeActionStatus(&request.Status.Actions[i], status)

		switch status.Phase {
		case dpv1alpha1.ActionPhaseCompleted:
			updateBackupStatusByActionStatus(&request.Status)
			continue
		case dpv1alpha1.ActionPhaseFailed:
			return r.updateStatusIfFailed(reqCtx, backup, request.Backup,
				fmt.Errorf("action %s failed, %s", act.GetName(), status.FailureReason))
		case dpv1alpha1.ActionPhaseRunning:
			// update status
			if err = r.Client.Status().Patch(reqCtx.Ctx, request.Backup, client.MergeFrom(backup)); err != nil {
				return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
			}
			return intctrlutil.Reconciled()
		}
	}

	// all actions completed, update backup status to completed
	request.Status.Phase = dpv1alpha1.BackupPhaseCompleted
	request.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now().UTC()}
	if !request.Status.StartTimestamp.IsZero() {
		// round the duration to a multiple of seconds.
		duration := request.Status.CompletionTimestamp.Sub(request.Status.StartTimestamp.Time).Round(time.Second)
		request.Status.Duration = &metav1.Duration{Duration: duration}
	}
	r.Recorder.Event(backup, corev1.EventTypeNormal, "CreatedBackup", "Completed backup")
	if err = r.Client.Status().Patch(reqCtx.Ctx, request.Backup, client.MergeFrom(backup)); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}
	return intctrlutil.Reconciled()
}

func mergeActionStatus(original, new *dpv1alpha1.ActionStatus) dpv1alpha1.ActionStatus {
	as := new.DeepCopy()
	if original.StartTimestamp != nil {
		as.StartTimestamp = original.StartTimestamp
	}
	return *as
}

func updateBackupStatusByActionStatus(backupStatus *dpv1alpha1.BackupStatus) {
	for _, act := range backupStatus.Actions {
		if act.TotalSize != "" && backupStatus.TotalSize == "" {
			backupStatus.TotalSize = act.TotalSize
		}
		if act.TimeRange != nil && backupStatus.TimeRange == nil {
			backupStatus.TimeRange = act.TimeRange
		}
	}
}

// handleCompletedPhase handles the backup object in completed phase.
// It will delete the reference workloads.
func (r *BackupReconciler) handleCompletedPhase(
	reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) (ctrl.Result, error) {
	if err := r.deleteExternalResources(reqCtx, backup); err != nil {
		return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
	}
	return intctrlutil.Reconciled()
}

func (r *BackupReconciler) updateStatusIfFailed(
	reqCtx intctrlutil.RequestCtx,
	original *dpv1alpha1.Backup,
	backup *dpv1alpha1.Backup,
	err error) (ctrl.Result, error) {
	sendWarningEventForError(r.Recorder, backup, err)
	backup.Status.Phase = dpv1alpha1.BackupPhaseFailed
	backup.Status.FailureReason = err.Error()
	if errUpdate := r.Client.Status().Patch(reqCtx.Ctx, backup, client.MergeFrom(original)); errUpdate != nil {
		return intctrlutil.CheckedRequeueWithError(errUpdate, reqCtx.Log, "")
	}
	return intctrlutil.CheckedRequeueWithError(err, reqCtx.Log, "")
}

// deleteExternalJobs deletes the external jobs.
func (r *BackupReconciler) deleteExternalJobs(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	jobs := &batchv1.JobList{}
	if err := r.Client.List(reqCtx.Ctx, jobs,
		client.InNamespace(backup.Namespace),
		client.MatchingLabels(dpbackup.BuildBackupWorkloadLabels(backup))); err != nil {
		return client.IgnoreNotFound(err)
	}

	deleteJob := func(job *batchv1.Job) error {
		if err := dputils.RemoveDataProtectionFinalizer(reqCtx.Ctx, r.Client, job); err != nil {
			return err
		}
		if !job.DeletionTimestamp.IsZero() {
			return nil
		}
		reqCtx.Log.V(1).Info("delete job", "job", job)
		if err := intctrlutil.BackgroundDeleteObject(r.Client, reqCtx.Ctx, job); err != nil {
			return err
		}
		return nil
	}

	for i := range jobs.Items {
		if err := deleteJob(&jobs.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *BackupReconciler) deleteVolumeSnapshots(reqCtx intctrlutil.RequestCtx,
	backup *dpv1alpha1.Backup) error {
	deleter := &dpbackup.Deleter{
		RequestCtx: reqCtx,
		Client:     r.Client,
	}
	return deleter.DeleteVolumeSnapshots(backup)
}

// deleteExternalStatefulSet deletes the external statefulSet.
func (r *BackupReconciler) deleteExternalStatefulSet(reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	key := client.ObjectKey{
		Namespace: backup.Namespace,
		Name:      backup.Name,
	}
	sts := &appsv1.StatefulSet{}
	if err := r.Client.Get(reqCtx.Ctx, key, sts); err != nil {
		return client.IgnoreNotFound(err)
	} else if !model.IsOwnerOf(backup, sts) {
		return nil
	}

	patch := client.MergeFrom(sts.DeepCopy())
	controllerutil.RemoveFinalizer(sts, dptypes.DataProtectionFinalizerName)
	if err := r.Client.Patch(reqCtx.Ctx, sts, patch); err != nil {
		return err
	}

	if !sts.DeletionTimestamp.IsZero() {
		return nil
	}

	reqCtx.Log.V(1).Info("delete statefulSet", "statefulSet", sts)
	return intctrlutil.BackgroundDeleteObject(r.Client, reqCtx.Ctx, sts)
}

// deleteExternalResources deletes the external workloads that execute backup.
// Currently, it only supports two types of workloads: statefulSet and job.
func (r *BackupReconciler) deleteExternalResources(
	reqCtx intctrlutil.RequestCtx, backup *dpv1alpha1.Backup) error {
	if err := r.deleteExternalStatefulSet(reqCtx, backup); err != nil {
		return err
	}
	return r.deleteExternalJobs(reqCtx, backup)
}

// getClusterObjectString gets the cluster object and convert it to string.
func getClusterObjectString(cluster *appsv1alpha1.Cluster) (*string, error) {
	// maintain only the cluster's spec and name/namespace.
	newCluster := &appsv1alpha1.Cluster{
		Spec: cluster.Spec,
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      cluster.Name,
		},
		TypeMeta: cluster.TypeMeta,
	}
	clusterBytes, err := json.Marshal(newCluster)
	if err != nil {
		return nil, err
	}
	clusterString := string(clusterBytes)
	return &clusterString, nil
}

// setClusterSnapshotAnnotation sets the snapshot of cluster to the backup's annotations.
func setClusterSnapshotAnnotation(backup *dpv1alpha1.Backup, cluster *appsv1alpha1.Cluster) error {
	clusterString, err := getClusterObjectString(cluster)
	if err != nil {
		return err
	}
	if clusterString == nil {
		return nil
	}
	if backup.Annotations == nil {
		backup.Annotations = map[string]string{}
	}
	backup.Annotations[constant.ClusterSnapshotAnnotationKey] = *clusterString
	return nil
}
