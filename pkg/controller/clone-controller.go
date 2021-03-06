package controller

import (
	"context"
	"crypto/rsa"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/token"
	"kubevirt.io/containerized-data-importer/pkg/util/cert/fetcher"
	"kubevirt.io/containerized-data-importer/pkg/util/cert/generator"
)

const (
	cloneControllerAgentName = "clone-controller"

	//AnnCloneRequest sets our expected annotation for a CloneRequest
	AnnCloneRequest = "k8s.io/CloneRequest"
	//AnnCloneOf is used to indicate that cloning was complete
	AnnCloneOf = "k8s.io/CloneOf"
	// AnnCloneToken is the annotation containing the clone token
	AnnCloneToken = "cdi.kubevirt.io/storage.clone.token"

	//CloneUniqueID is used as a special label to be used when we search for the pod
	CloneUniqueID = "cdi.kubevirt.io/storage.clone.cloneUniqeId"

	// ErrIncompatiblePVC provides a const to indicate a clone is not possible due to an incompatible PVC
	ErrIncompatiblePVC = "ErrIncompatiblePVC"

	// APIServerPublicKeyDir is the path to the apiserver public key dir
	APIServerPublicKeyDir = "/var/run/cdi/apiserver/key"

	// APIServerPublicKeyPath is the path to the apiserver public key
	APIServerPublicKeyPath = APIServerPublicKeyDir + "/id_rsa.pub"

	// CloneSucceededPVC provides a const to indicate a clone to the PVC succeeded
	CloneSucceededPVC = "CloneSucceeded"

	cloneSourcePodFinalizer = "cdi.kubevirt.io/cloneSource"

	cloneTokenLeeway = 10 * time.Second

	uploadClientCertDuration = 365 * 24 * time.Hour
)

// CloneReconciler members
type CloneReconciler struct {
	Client              client.Client
	Scheme              *runtime.Scheme
	K8sClient           kubernetes.Interface
	recorder            record.EventRecorder
	clientCertGenerator generator.CertGenerator
	serverCAFetcher     fetcher.CertBundleFetcher
	Log                 logr.Logger
	tokenValidator      token.Validator
	Image               string
	Verbose             string
	PullPolicy          string
}

// NewCloneController creates a new instance of the config controller.
func NewCloneController(mgr manager.Manager,
	k8sClient kubernetes.Interface,
	log logr.Logger,
	image, pullPolicy,
	verbose string,
	clientCertGenerator generator.CertGenerator,
	serverCAFetcher fetcher.CertBundleFetcher,
	apiServerKey *rsa.PublicKey) (controller.Controller, error) {
	reconciler := &CloneReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Log:                 log.WithName("clone-controller"),
		tokenValidator:      newCloneTokenValidator(apiServerKey),
		Image:               image,
		Verbose:             verbose,
		PullPolicy:          pullPolicy,
		recorder:            mgr.GetEventRecorderFor("clone-controller"),
		K8sClient:           k8sClient,
		clientCertGenerator: clientCertGenerator,
		serverCAFetcher:     serverCAFetcher,
	}
	cloneController, err := controller.New("clone-controller", mgr, controller.Options{
		Reconciler: reconciler,
	})
	if err != nil {
		return nil, err
	}
	if err := addCloneControllerWatches(mgr, cloneController); err != nil {
		return nil, err
	}
	return cloneController, nil
}

// addConfigControllerWatches sets up the watches used by the config controller.
func addCloneControllerWatches(mgr manager.Manager, cloneController controller.Controller) error {
	// Setup watches
	if err := cloneController.Watch(&source.Kind{Type: &corev1.PersistentVolumeClaim{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}
	if err := cloneController.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		OwnerType:    &corev1.PersistentVolumeClaim{},
		IsController: true,
	}); err != nil {
		return err
	}
	return nil
}

func newCloneTokenValidator(key *rsa.PublicKey) token.Validator {
	return token.NewValidator(common.CloneTokenIssuer, key, cloneTokenLeeway)
}

func (r *CloneReconciler) shouldReconcile(pvc *corev1.PersistentVolumeClaim) bool {
	return checkPVC(pvc, AnnCloneRequest) && !metav1.HasAnnotation(pvc.ObjectMeta, AnnCloneOf)
}

// Reconcile the reconcile loop for host assisted clone pvc.
func (r *CloneReconciler) Reconcile(req reconcile.Request) (reconcile.Result, error) {
	// Get the PVC.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(context.TODO(), req.NamespacedName, pvc); err != nil {
		if k8serrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	log := r.Log.WithValues("PVC", req.NamespacedName)
	log.V(1).Info("reconciling Clone PVCs")
	if !r.shouldReconcile(pvc) {
		log.V(1).Info("Should not reconcile this PVC", "checkPVC(AnnCloneRequest)", checkPVC(pvc, AnnCloneRequest), "NOT has annotation(AnnCloneOf)", !metav1.HasAnnotation(pvc.ObjectMeta, AnnCloneOf), "has finalizer?", r.hasFinalizer(pvc, cloneSourcePodFinalizer))
		if r.hasFinalizer(pvc, cloneSourcePodFinalizer) {
			// Clone completed, remove source pod and finalizer.
			if err := r.cleanup(pvc, log); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	ready, err := r.waitTargetPodRunningOrSucceeded(pvc, log)
	if err != nil {
		return reconcile.Result{}, errors.Wrap(err, "error ensuring target upload pod running")
	}

	if !ready {
		log.V(3).Info("Upload pod not ready yet for PVC")
		return reconcile.Result{}, nil
	}

	sourcePod, err := r.findCloneSourcePod(pvc)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.reconcileSourcePod(sourcePod, pvc, log); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.updatePvcFromPod(sourcePod, pvc, log); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (r *CloneReconciler) reconcileSourcePod(sourcePod *corev1.Pod, pvc *corev1.PersistentVolumeClaim, log logr.Logger) error {
	if sourcePod == nil {
		if err := r.validateSourceAndTarget(pvc); err != nil {
			return err
		}

		clientName, ok := pvc.Annotations[AnnUploadClientName]
		if !ok {
			return errors.Errorf("PVC %s/%s missing required %s annotation", pvc.Namespace, pvc.Name, AnnUploadClientName)
		}

		sourcePod, err := r.CreateCloneSourcePod(r.Image, r.PullPolicy, clientName, pvc, log)
		if err != nil {
			return err
		}
		log.V(3).Info("Created source pod ", "sourcePod.Namespace", sourcePod.Namespace, "sourcePod.Name", sourcePod.Name)
	}
	return nil
}

func (r *CloneReconciler) updatePvcFromPod(sourcePod *corev1.Pod, pvc *corev1.PersistentVolumeClaim, log logr.Logger) error {
	currentPvcCopy := pvc.DeepCopyObject()
	log.V(1).Info("Updating PVC from pod")

	pvc = r.addFinalizer(pvc, cloneSourcePodFinalizer)

	log.V(3).Info("Pod phase for PVC", "PVC phase", pvc.Annotations[AnnPodPhase])

	if podSucceededFromPVC(pvc) && pvc.Annotations[AnnCloneOf] != "true" {
		log.V(1).Info("Adding CloneOf annotation to PVC")
		pvc.Annotations[AnnCloneOf] = "true"
		r.recorder.Event(pvc, corev1.EventTypeNormal, CloneSucceededPVC, "Clone Successful")
	}
	if sourcePod != nil && sourcePod.Status.ContainerStatuses != nil {
		// update pvc annotation tracking pod restarts only if the source pod restart count is greater
		// see the same in upload-controller
		annPodRestarts, _ := strconv.Atoi(pvc.Annotations[AnnPodRestarts])
		podRestarts := int(sourcePod.Status.ContainerStatuses[0].RestartCount)
		if podRestarts > annPodRestarts {
			pvc.Annotations[AnnPodRestarts] = strconv.Itoa(podRestarts)
		}
	}

	if !reflect.DeepEqual(currentPvcCopy, pvc) {
		return r.updatePVC(pvc)
	}
	return nil
}

func (r *CloneReconciler) updatePVC(pvc *corev1.PersistentVolumeClaim) error {
	if err := r.Client.Update(context.TODO(), pvc); err != nil {
		return err
	}
	return nil
}

func (r *CloneReconciler) waitTargetPodRunningOrSucceeded(pvc *corev1.PersistentVolumeClaim, log logr.Logger) (bool, error) {
	rs, ok := pvc.Annotations[AnnPodReady]
	if !ok {
		log.V(3).Info("clone target pod not ready")
		return false, nil
	}

	ready, err := strconv.ParseBool(rs)
	if err != nil {
		return false, errors.Wrapf(err, "error parsing %s annotation", AnnPodReady)
	}

	if !ready {
		log.V(3).Info("clone target pod not ready")
		return podSucceededFromPVC(pvc), nil
	}

	return true, nil
}

func (r *CloneReconciler) findCloneSourcePod(pvc *corev1.PersistentVolumeClaim) (*corev1.Pod, error) {
	isCloneRequest, sourceNamespace, _ := ParseCloneRequestAnnotation(pvc)
	if !isCloneRequest {
		return nil, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{
			CloneUniqueID: string(pvc.GetUID()) + "-source-pod",
		},
	})
	if err != nil {
		return nil, errors.Wrap(err, "error creating label selector")
	}

	podList := &corev1.PodList{}
	if err := r.Client.List(context.TODO(), podList, &client.ListOptions{Namespace: sourceNamespace, LabelSelector: selector}); err != nil {
		return nil, errors.Wrap(err, "error listing pods")
	}

	if len(podList.Items) > 1 {
		return nil, errors.Errorf("multiple source pods found for clone PVC %s/%s", pvc.Namespace, pvc.Name)
	}

	if len(podList.Items) == 0 {
		return nil, nil
	}

	return &podList.Items[0], nil
}

func (r *CloneReconciler) validateSourceAndTarget(targetPvc *corev1.PersistentVolumeClaim) error {
	sourcePvc, err := r.getCloneRequestSourcePVC(targetPvc)
	if err != nil {
		return err
	}

	if err = validateCloneToken(r.tokenValidator, sourcePvc, targetPvc); err != nil {
		return err
	}

	return ValidateCanCloneSourceAndTargetSpec(&sourcePvc.Spec, &targetPvc.Spec)
}

func (r *CloneReconciler) addFinalizer(pvc *corev1.PersistentVolumeClaim, name string) *corev1.PersistentVolumeClaim {
	if r.hasFinalizer(pvc, name) {
		return pvc
	}

	pvc.Finalizers = append(pvc.Finalizers, name)
	return pvc
}

func (r *CloneReconciler) removeFinalizer(pvc *corev1.PersistentVolumeClaim, name string) *corev1.PersistentVolumeClaim {
	if !r.hasFinalizer(pvc, name) {
		return pvc
	}

	var finalizers []string
	for _, f := range pvc.Finalizers {
		if f != name {
			finalizers = append(finalizers, f)
		}
	}

	pvc.Finalizers = finalizers
	return pvc
}

func (r *CloneReconciler) hasFinalizer(object metav1.Object, value string) bool {
	for _, f := range object.GetFinalizers() {
		if f == value {
			return true
		}
	}
	return false
}

// returns the CloneRequest string which contains the pvc name (and namespace) from which we want to clone the image.
func (r *CloneReconciler) getCloneRequestSourcePVC(pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolumeClaim, error) {
	exists, namespace, name := ParseCloneRequestAnnotation(pvc)
	if !exists {
		return nil, errors.New("error parsing clone request annotation")
	}
	pvc = &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, pvc); err != nil {
		return nil, errors.Wrap(err, "error getting clone source PVC")
	}
	return pvc, nil
}

func (r *CloneReconciler) cleanup(pvc *corev1.PersistentVolumeClaim, log logr.Logger) error {
	log.V(3).Info("Cleaning up for PVC", "pvc.Namespace", pvc.Namespace, "pvc.Name", pvc.Name)

	pod, err := r.findCloneSourcePod(pvc)
	if err != nil {
		return err
	}

	if pod != nil && pod.DeletionTimestamp == nil {
		if podSucceededFromPVC(pvc) && pod.Status.Phase == corev1.PodRunning {
			log.V(3).Info("Clone succeeded, waiting for source pod to stop running", "pod.Namespace", pod.Namespace, "pod.Name", pod.Name)
			return nil
		}

		if err = r.Client.Delete(context.TODO(), pod); err != nil {
			if !k8serrors.IsNotFound(err) {
				return errors.Wrap(err, "error deleting clone source pod")
			}
		}
	}

	return r.updatePVC(r.removeFinalizer(pvc, cloneSourcePodFinalizer))
}

// CreateCloneSourcePod creates our cloning src pod which will be used for out of band cloning to read the contents of the src PVC
func (r *CloneReconciler) CreateCloneSourcePod(image, pullPolicy, clientName string, pvc *corev1.PersistentVolumeClaim, log logr.Logger) (*corev1.Pod, error) {
	exists, sourcePvcNamespace, sourcePvcName := ParseCloneRequestAnnotation(pvc)
	if !exists {
		return nil, errors.Errorf("bad CloneRequest Annotation")
	}

	ownerKey, err := cache.MetaNamespaceKeyFunc(pvc)
	if err != nil {
		return nil, errors.Wrap(err, "error getting cache key")
	}

	clientCert, clientKey, err := r.clientCertGenerator.MakeClientCert(clientName, nil, uploadClientCertDuration)
	if err != nil {
		return nil, err
	}

	serverCABundle, err := r.serverCAFetcher.BundleBytes()
	if err != nil {
		return nil, err
	}

	podResourceRequirements, err := GetDefaultPodResourceRequirements(r.Client)
	if err != nil {
		return nil, err
	}

	pod := MakeCloneSourcePodSpec(image, pullPolicy, sourcePvcName, sourcePvcNamespace, ownerKey, clientKey, clientCert, serverCABundle, pvc, podResourceRequirements)

	if err := r.Client.Create(context.TODO(), pod); err != nil {
		return nil, errors.Wrap(err, "source pod API create errored")
	}

	log.V(1).Info("cloning source pod (image) created\n", "pod.Namespace", pod.Namespace, "pod.Name", pod.Name, "image", image)

	return pod, nil
}

func getCloneSourcePodName(targetPvc *corev1.PersistentVolumeClaim) string {
	return string(targetPvc.GetUID()) + "-source-pod"
}

// MakeCloneSourcePodSpec creates and returns the clone source pod spec based on the target pvc.
func MakeCloneSourcePodSpec(image, pullPolicy, sourcePvcName, sourcePvcNamespace, ownerRefAnno string,
	clientKey, clientCert, serverCACert []byte, targetPvc *corev1.PersistentVolumeClaim, resourceRequirements *corev1.ResourceRequirements) *corev1.Pod {

	var ownerID string
	podName := getCloneSourcePodName(targetPvc)
	url := GetUploadServerURL(targetPvc.Namespace, targetPvc.Name, common.UploadPathSync)
	pvcOwner := metav1.GetControllerOf(targetPvc)
	if pvcOwner != nil && pvcOwner.Kind == "DataVolume" {
		ownerID = string(pvcOwner.UID)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: sourcePvcNamespace,
			Annotations: map[string]string{
				AnnCreatedBy: "yes",
				AnnOwnerRef:  ownerRefAnno,
			},
			Labels: map[string]string{
				common.CDILabelKey:       common.CDILabelValue, //filtered by the podInformer
				common.CDIComponentLabel: common.ClonerSourcePodName,
				// this label is used when searching for a pvc's cloner source pod.
				CloneUniqueID:          getCloneSourcePodName(targetPvc),
				common.PrometheusLabel: "",
			},
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &[]int64{0}[0],
			},
			Containers: []corev1.Container{
				{
					Name:            common.ClonerSourcePodName,
					Image:           image,
					ImagePullPolicy: corev1.PullPolicy(pullPolicy),
					Env: []corev1.EnvVar{
						/*
						 Easier to just stick key/certs in env vars directly no.
						 Maybe revisit when we fix the "naming things" problem.
						*/
						{
							Name:  "CLIENT_KEY",
							Value: string(clientKey),
						},
						{
							Name:  "CLIENT_CERT",
							Value: string(clientCert),
						},
						{
							Name:  "SERVER_CA_CERT",
							Value: string(serverCACert),
						},
						{
							Name:  "UPLOAD_URL",
							Value: url,
						},
						{
							Name:  common.OwnerUID,
							Value: ownerID,
						},
					},
					Ports: []corev1.ContainerPort{
						{
							Name:          "metrics",
							ContainerPort: 8443,
							Protocol:      corev1.ProtocolTCP,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Volumes: []corev1.Volume{
				{
					Name: DataVolName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: sourcePvcName,
							ReadOnly:  true,
						},
					},
				},
			},
		},
	}

	if resourceRequirements != nil {
		pod.Spec.Containers[0].Resources = *resourceRequirements
	}

	var volumeMode corev1.PersistentVolumeMode
	var addVars []corev1.EnvVar

	if targetPvc.Spec.VolumeMode != nil {
		volumeMode = *targetPvc.Spec.VolumeMode
	} else {
		volumeMode = corev1.PersistentVolumeFilesystem
	}

	if volumeMode == corev1.PersistentVolumeBlock {
		pod.Spec.Containers[0].VolumeDevices = addVolumeDevices()
		addVars = []corev1.EnvVar{
			{
				Name:  "VOLUME_MODE",
				Value: "block",
			},
			{
				Name:  "MOUNT_POINT",
				Value: common.WriteBlockPath,
			},
		}
	} else {
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{
				Name:      DataVolName,
				MountPath: common.ClonerMountPath,
			},
		}
		addVars = []corev1.EnvVar{
			{
				Name:  "VOLUME_MODE",
				Value: "filesystem",
			},
			{
				Name:  "MOUNT_POINT",
				Value: common.ClonerMountPath,
			},
		}
	}

	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, addVars...)

	return pod
}

func validateCloneToken(validator token.Validator, source, target *corev1.PersistentVolumeClaim) error {
	tok, ok := target.Annotations[AnnCloneToken]
	if !ok {
		return errors.New("clone token missing")
	}

	tokenData, err := validator.Validate(tok)
	if err != nil {
		return errors.Wrap(err, "error verifying token")
	}

	if tokenData.Operation != token.OperationClone ||
		tokenData.Name != source.Name ||
		tokenData.Namespace != source.Namespace ||
		tokenData.Resource.Resource != "persistentvolumeclaims" ||
		tokenData.Params["targetNamespace"] != target.Namespace ||
		tokenData.Params["targetName"] != target.Name {
		return errors.New("invalid token")
	}

	return nil
}

// ParseCloneRequestAnnotation parses the clone request annotation
func ParseCloneRequestAnnotation(pvc *corev1.PersistentVolumeClaim) (exists bool, namespace, name string) {
	var ann string
	ann, exists = pvc.Annotations[AnnCloneRequest]
	if !exists {
		return
	}

	sp := strings.Split(ann, "/")
	if len(sp) != 2 {
		exists = false
		return
	}

	namespace, name = sp[0], sp[1]
	return
}

// ValidateCanCloneSourceAndTargetSpec validates the specs passed in are compatible for cloning.
func ValidateCanCloneSourceAndTargetSpec(sourceSpec, targetSpec *corev1.PersistentVolumeClaimSpec) error {
	sourceRequest := sourceSpec.Resources.Requests[corev1.ResourceStorage]
	targetRequest := targetSpec.Resources.Requests[corev1.ResourceStorage]
	// Verify that the target PVC size is equal or larger than the source.
	if sourceRequest.Value() > targetRequest.Value() {
		return errors.New("target resources requests storage size is smaller than the source")
	}
	// Verify that the source and target volume modes are the same.
	sourceVolumeMode := corev1.PersistentVolumeFilesystem
	if sourceSpec.VolumeMode != nil && *sourceSpec.VolumeMode == corev1.PersistentVolumeBlock {
		sourceVolumeMode = corev1.PersistentVolumeBlock
	}
	targetVolumeMode := corev1.PersistentVolumeFilesystem
	if targetSpec.VolumeMode != nil && *targetSpec.VolumeMode == corev1.PersistentVolumeBlock {
		targetVolumeMode = corev1.PersistentVolumeBlock
	}
	if sourceVolumeMode != targetVolumeMode {
		return fmt.Errorf("source volumeMode (%s) and target volumeMode (%s) do not match",
			sourceVolumeMode, targetVolumeMode)
	}
	// Can clone.
	return nil
}
