// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package istio

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/istio-ecosystem/sail-operator/api/v1alpha1"
	"github.com/istio-ecosystem/sail-operator/pkg/config"
	"github.com/istio-ecosystem/sail-operator/pkg/errlist"
	"github.com/istio-ecosystem/sail-operator/pkg/helm"
	"github.com/istio-ecosystem/sail-operator/pkg/kube"
	"github.com/istio-ecosystem/sail-operator/pkg/profiles"
	"github.com/istio-ecosystem/sail-operator/pkg/reconciler"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"istio.io/istio/pkg/ptr"
)

// Reconciler reconciles an Istio object
type Reconciler struct {
	ResourceDirectory string
	DefaultProfile    string
	client.Client
	Scheme *runtime.Scheme
}

func NewReconciler(client client.Client, scheme *runtime.Scheme, resourceDir string, defaultProfile string) *Reconciler {
	return &Reconciler{
		ResourceDirectory: resourceDir,
		DefaultProfile:    defaultProfile,
		Client:            client,
		Scheme:            scheme,
	}
}

// +kubebuilder:rbac:groups=operator.istio.io,resources=istios,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.istio.io,resources=istios/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=operator.istio.io,resources=istios/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *Reconciler) Reconcile(ctx context.Context, istio *v1alpha1.Istio) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	log.Info("Reconciling")
	result, reconcileErr := r.doReconcile(ctx, istio)

	log.Info("Reconciliation done. Updating status.")
	statusErr := r.updateStatus(ctx, istio, reconcileErr)

	return result, errors.Join(reconcileErr, statusErr)
}

// doReconcile is the function that actually reconciles the Istio object. Any error reported by this
// function should get reported in the status of the Istio object by the caller.
func (r *Reconciler) doReconcile(ctx context.Context, istio *v1alpha1.Istio) (result ctrl.Result, err error) {
	if istio.Spec.Version == "" {
		return ctrl.Result{}, fmt.Errorf("no spec.version set")
	}
	if istio.Spec.Namespace == "" {
		return ctrl.Result{}, fmt.Errorf("no spec.namespace set")
	}

	var values *v1alpha1.Values
	if values, err = computeIstioRevisionValues(istio, r.DefaultProfile, r.ResourceDirectory); err != nil {
		return ctrl.Result{}, err
	}

	if err = r.reconcileActiveRevision(ctx, istio, values); err != nil {
		return ctrl.Result{}, err
	}

	return r.pruneInactiveRevisions(ctx, istio)
}

func (r *Reconciler) reconcileActiveRevision(ctx context.Context, istio *v1alpha1.Istio, values *v1alpha1.Values) error {
	log := logf.FromContext(ctx)

	activeRevisionName := getActiveRevisionName(istio)
	log = log.WithValues("IstioRevision", activeRevisionName)

	rev, err := r.getActiveRevision(ctx, istio)
	if err == nil {
		// update
		rev.Spec.Version = istio.Spec.Version
		rev.Spec.Values = values
		log.Info("Updating IstioRevision")
		err = r.Client.Update(ctx, &rev)
	} else if apierrors.IsNotFound(err) {
		// create new
		rev = v1alpha1.IstioRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name: activeRevisionName,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         v1alpha1.GroupVersion.String(),
						Kind:               v1alpha1.IstioKind,
						Name:               istio.Name,
						UID:                istio.UID,
						Controller:         ptr.Of(true),
						BlockOwnerDeletion: ptr.Of(true),
					},
				},
			},
			Spec: v1alpha1.IstioRevisionSpec{
				Version:   istio.Spec.Version,
				Namespace: istio.Spec.Namespace,
				Values:    values,
			},
		}
		log.Info("Creating IstioRevision")
		err = r.Client.Create(ctx, &rev)
	}
	return err
}

func (r *Reconciler) pruneInactiveRevisions(ctx context.Context, istio *v1alpha1.Istio) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	revisions, err := r.getRevisions(ctx, istio)
	if err != nil {
		return ctrl.Result{}, err
	}

	// the following code does two things:
	// - prunes revisions whose grace period has expired
	// - finds the time when the next revision is to be pruned
	var nextPruneTimestamp *time.Time
	for _, rev := range revisions {
		if isActiveRevision(istio, &rev) {
			log.V(2).Info("IstioRevision is the active revision", "IstioRevision", rev.Name)
			continue
		}
		inUseCondition := rev.Status.GetCondition(v1alpha1.IstioRevisionConditionInUse)
		inUse := inUseCondition.Status == metav1.ConditionTrue
		if inUse {
			log.V(2).Info("IstioRevision is in use", "IstioRevision", rev.Name)
			continue
		}

		pruneTimestamp := inUseCondition.LastTransitionTime.Time.Add(getPruningGracePeriod(istio))
		expired := pruneTimestamp.Before(time.Now())
		if expired {
			log.Info("Deleting expired IstioRevision", "IstioRevision", rev.Name)
			err = r.Client.Delete(ctx, &rev)
			if err != nil {
				return ctrl.Result{}, err
			}
		} else {
			log.V(2).Info("IstioRevision is not in use, but hasn't yet expired", "IstioRevision", rev.Name, "InUseLastTransitionTime", inUseCondition.LastTransitionTime)
			if nextPruneTimestamp == nil || nextPruneTimestamp.After(pruneTimestamp) {
				nextPruneTimestamp = &pruneTimestamp
			}
		}
	}
	if nextPruneTimestamp == nil {
		log.V(2).Info("No IstioRevisions to prune")
		return ctrl.Result{}, nil
	}

	requeueAfter := time.Until(*nextPruneTimestamp)
	log.Info("Requeueing Istio resource for cleanup of expired IstioRevision", "RequeueAfter", requeueAfter)
	// requeue so that we prune the next revision at the right time (if we didn't, we would prune it when
	// something else triggers another reconciliation)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func getPruningGracePeriod(istio *v1alpha1.Istio) time.Duration {
	strategy := istio.Spec.UpdateStrategy
	period := int64(v1alpha1.DefaultRevisionDeletionGracePeriodSeconds)
	if strategy != nil && strategy.InactiveRevisionDeletionGracePeriodSeconds != nil {
		period = *strategy.InactiveRevisionDeletionGracePeriodSeconds
	}
	if period < v1alpha1.MinRevisionDeletionGracePeriodSeconds {
		period = v1alpha1.MinRevisionDeletionGracePeriodSeconds
	}
	return time.Duration(period) * time.Second
}

func (r *Reconciler) getActiveRevision(ctx context.Context, istio *v1alpha1.Istio) (v1alpha1.IstioRevision, error) {
	rev := v1alpha1.IstioRevision{}
	err := r.Client.Get(ctx, getActiveRevisionKey(istio), &rev)
	return rev, err
}

func (r *Reconciler) getRevisions(ctx context.Context, istio *v1alpha1.Istio) ([]v1alpha1.IstioRevision, error) {
	revList := v1alpha1.IstioRevisionList{}
	if err := r.Client.List(ctx, &revList); err != nil {
		return nil, err
	}

	var revisions []v1alpha1.IstioRevision
	for _, rev := range revList.Items {
		if isRevisionOwnedByIstio(rev, istio) {
			revisions = append(revisions, rev)
		}
	}
	return revisions, nil
}

func isRevisionOwnedByIstio(rev v1alpha1.IstioRevision, istio *v1alpha1.Istio) bool {
	if istio.UID == "" {
		panic(fmt.Sprintf("No UID set in Istio %q; did you forget to set it in your test?", istio.Name))
	}
	for _, owner := range rev.OwnerReferences {
		if owner.UID == istio.UID {
			return true
		}
	}
	return false
}

func isActiveRevision(istio *v1alpha1.Istio, rev *v1alpha1.IstioRevision) bool {
	return rev.Name == getActiveRevisionName(istio)
}

func getActiveRevisionKey(istio *v1alpha1.Istio) types.NamespacedName {
	return types.NamespacedName{
		Name: getActiveRevisionName(istio),
	}
}

func getActiveRevisionName(istio *v1alpha1.Istio) string {
	var strategy v1alpha1.UpdateStrategyType
	if istio.Spec.UpdateStrategy != nil {
		strategy = istio.Spec.UpdateStrategy.Type
	}

	switch strategy {
	default:
		fallthrough
	case v1alpha1.UpdateStrategyTypeInPlace:
		return istio.Name
	case v1alpha1.UpdateStrategyTypeRevisionBased:
		return istio.Name + "-" + strings.ReplaceAll(istio.Spec.Version, ".", "-")
	}
}

func computeIstioRevisionValues(istio *v1alpha1.Istio, defaultProfile string, resourceDir string) (*v1alpha1.Values, error) {
	// get userValues from Istio.spec.values
	userValues := istio.Spec.Values

	// apply image digests from configuration, if not already set by user
	userValues = applyImageDigests(istio, userValues, config.Config)

	// apply userValues on top of defaultValues from profiles
	mergedHelmValues, err := profiles.Apply(getProfilesDir(resourceDir, istio), defaultProfile, istio.Spec.Profile, helm.FromValues(userValues))
	if err != nil {
		return nil, err
	}

	values, err := helm.ToValues(mergedHelmValues, &v1alpha1.Values{})
	if err != nil {
		return nil, err
	}

	// override values that are not configurable by the user
	return applyOverrides(istio, values)
}

func getProfilesDir(resourceDir string, istio *v1alpha1.Istio) string {
	return path.Join(resourceDir, istio.Spec.Version, "profiles")
}

func applyOverrides(istio *v1alpha1.Istio, values *v1alpha1.Values) (*v1alpha1.Values, error) {
	revisionName := getActiveRevisionName(istio)

	// Set revision name to "" if revision name is "default". This is a temporary fix until we fix the injection
	// mutatingwebhook manifest; the webhook performs injection on namespaces labeled with "istio-injection: enabled"
	// only when revision is "", but not also for "default", which it should, since elsewhere in the same manifest,
	// the "" revision is mapped to "default".
	if revisionName == v1alpha1.DefaultRevision {
		revisionName = ""
	}
	values.Revision = revisionName

	if values.Global == nil {
		values.Global = &v1alpha1.GlobalConfig{}
	}
	values.Global.IstioNamespace = istio.Spec.Namespace
	return values, nil
}

func applyImageDigests(istio *v1alpha1.Istio, values *v1alpha1.Values, config config.OperatorConfig) *v1alpha1.Values {
	imageDigests, digestsDefined := config.ImageDigests[istio.Spec.Version]
	// if we don't have default image digests defined for this version, it's a no-op
	if !digestsDefined {
		return values
	}

	if values == nil {
		values = &v1alpha1.Values{}
	}

	// set image digests for components unless they've been configured by the user
	if values.Pilot == nil {
		values.Pilot = &v1alpha1.PilotConfig{}
	}
	if values.Pilot.Image == "" && values.Pilot.Hub == "" && values.Pilot.Tag == nil {
		values.Pilot.Image = imageDigests.IstiodImage
	}

	if values.Global == nil {
		values.Global = &v1alpha1.GlobalConfig{}
	}

	if values.Global.Proxy == nil {
		values.Global.Proxy = &v1alpha1.ProxyConfig{}
	}
	if values.Global.Proxy.Image == "" {
		values.Global.Proxy.Image = imageDigests.ProxyImage
	}

	if values.Global.ProxyInit == nil {
		values.Global.ProxyInit = &v1alpha1.ProxyInitConfig{}
	}
	if values.Global.ProxyInit.Image == "" {
		values.Global.ProxyInit.Image = imageDigests.ProxyImage
	}

	// TODO: add this once the API supports ambient
	// if !hasUserDefinedImage("ztunnel", values) {
	// 	values.ZTunnel.Image = imageDigests.ZTunnelImage
	// }
	return values
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			LogConstructor: func(req *reconcile.Request) logr.Logger {
				log := mgr.GetLogger().WithName("ctrlr").WithName("istio")
				if req != nil {
					log = log.WithValues("Istio", req.Name)
				}
				return log
			},
		}).
		For(&v1alpha1.Istio{}).
		Owns(&v1alpha1.IstioRevision{}).
		Complete(reconciler.NewStandardReconciler(r.Client, &v1alpha1.Istio{}, r.Reconcile))
}

func (r *Reconciler) determineStatus(ctx context.Context, istio *v1alpha1.Istio, reconcileErr error) (v1alpha1.IstioStatus, error) {
	var errs errlist.Builder
	status := *istio.Status.DeepCopy()
	status.ObservedGeneration = istio.Generation

	// set Reconciled and Ready conditions
	if reconcileErr != nil {
		status.SetCondition(v1alpha1.IstioCondition{
			Type:    v1alpha1.IstioConditionReconciled,
			Status:  metav1.ConditionFalse,
			Reason:  v1alpha1.IstioReasonReconcileError,
			Message: reconcileErr.Error(),
		})
		status.SetCondition(v1alpha1.IstioCondition{
			Type:    v1alpha1.IstioConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  v1alpha1.IstioReasonReconcileError,
			Message: "cannot determine readiness due to reconciliation error",
		})
		status.State = v1alpha1.IstioReasonReconcileError
	} else {
		rev, err := r.getActiveRevision(ctx, istio)
		if apierrors.IsNotFound(err) {
			revisionNotFound := func(conditionType v1alpha1.IstioConditionType) v1alpha1.IstioCondition {
				return v1alpha1.IstioCondition{
					Type:    conditionType,
					Status:  metav1.ConditionFalse,
					Reason:  v1alpha1.IstioReasonRevisionNotFound,
					Message: "active IstioRevision not found",
				}
			}

			status.SetCondition(revisionNotFound(v1alpha1.IstioConditionReconciled))
			status.SetCondition(revisionNotFound(v1alpha1.IstioConditionReady))
			status.State = v1alpha1.IstioReasonRevisionNotFound
		} else if err == nil {
			status.SetCondition(convertCondition(rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReconciled)))
			status.SetCondition(convertCondition(rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReady)))
			status.State = convertConditionReason(rev.Status.State)
		} else {
			activeRevisionGetFailed := func(conditionType v1alpha1.IstioConditionType) v1alpha1.IstioCondition {
				return v1alpha1.IstioCondition{
					Type:    conditionType,
					Status:  metav1.ConditionUnknown,
					Reason:  v1alpha1.IstioReasonFailedToGetActiveRevision,
					Message: fmt.Sprintf("failed to get active IstioRevision: %s", err),
				}
			}
			status.SetCondition(activeRevisionGetFailed(v1alpha1.IstioConditionReconciled))
			status.SetCondition(activeRevisionGetFailed(v1alpha1.IstioConditionReady))
			status.State = v1alpha1.IstioReasonFailedToGetActiveRevision
			errs.Add(err)
		}
	}

	// count the ready, in-use, and total revisions
	if revisions, err := r.getRevisions(ctx, istio); err == nil {
		status.Revisions.Total = int32(len(revisions))
		status.Revisions.Ready = 0
		status.Revisions.InUse = 0
		for _, rev := range revisions {
			if rev.Status.GetCondition(v1alpha1.IstioRevisionConditionReady).Status == metav1.ConditionTrue {
				status.Revisions.Ready++
			}
			if rev.Status.GetCondition(v1alpha1.IstioRevisionConditionInUse).Status == metav1.ConditionTrue {
				status.Revisions.InUse++
			}
		}
	} else {
		status.Revisions.Total = -1
		status.Revisions.Ready = -1
		status.Revisions.InUse = -1
		errs.Add(err)
	}
	return status, errs.Error()
}

func (r *Reconciler) updateStatus(ctx context.Context, istio *v1alpha1.Istio, reconcileErr error) error {
	var errs errlist.Builder
	status, err := r.determineStatus(ctx, istio, reconcileErr)
	errs.Add(err)

	if !reflect.DeepEqual(istio.Status, status) {
		errs.Add(r.Client.Status().Patch(ctx, istio, kube.NewStatusPatch(status)))
	}
	return errs.Error()
}

func convertCondition(condition v1alpha1.IstioRevisionCondition) v1alpha1.IstioCondition {
	return v1alpha1.IstioCondition{
		Type:    convertConditionType(condition),
		Status:  condition.Status,
		Reason:  convertConditionReason(condition.Reason),
		Message: condition.Message,
	}
}

func convertConditionType(condition v1alpha1.IstioRevisionCondition) v1alpha1.IstioConditionType {
	switch condition.Type {
	case v1alpha1.IstioRevisionConditionReconciled:
		return v1alpha1.IstioConditionReconciled
	case v1alpha1.IstioRevisionConditionReady:
		return v1alpha1.IstioConditionReady
	default:
		panic(fmt.Sprintf("can't convert IstioRevisionConditionType: %s", condition.Type))
	}
}

func convertConditionReason(reason v1alpha1.IstioRevisionConditionReason) v1alpha1.IstioConditionReason {
	switch reason {
	case "":
		return ""
	case v1alpha1.IstioRevisionReasonIstiodNotReady:
		return v1alpha1.IstioReasonIstiodNotReady
	case v1alpha1.IstioRevisionReasonHealthy:
		return v1alpha1.IstioReasonHealthy
	case v1alpha1.IstioRevisionReasonReadinessCheckFailed:
		return v1alpha1.IstioReasonReadinessCheckFailed
	case v1alpha1.IstioRevisionReasonReconcileError:
		return v1alpha1.IstioReasonReconcileError
	default:
		panic(fmt.Sprintf("can't convert IstioRevisionConditionReason: %s", reason))
	}
}
