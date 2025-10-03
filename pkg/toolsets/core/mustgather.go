package core

import (
	"context"
	"crypto/rand"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/containers/kubernetes-mcp-server/pkg/api"
	internalk8s "github.com/containers/kubernetes-mcp-server/pkg/kubernetes"
	"github.com/google/jsonschema-go/jsonschema"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	defaultGatherSourceDir = "/must-gather/"
	defaultMustGatherImage = "registry.redhat.io/openshift4/ose-must-gather:latest"
	defaultGatherCmd       = "/usr/bin/gather"
	// annotation to look for in ClusterServiceVersions and ClusterOperators when using --all-images
	mgAnnotation = "operators.openshift.io/must-gather-image"
)

func initMustGatherPlan(o internalk8s.Openshift) []api.ServerTool {
	// must-gather collection plan is only applicable to OpenShift clusters
	if !o.IsOpenShift(context.Background()) {
		return []api.ServerTool{}
	}

	return []api.ServerTool{{
		Tool: api.Tool{
			Name:        "plan_mustgather",
			Description: "Plan for collecting a must-gather archive from an OpenShift cluster, must-gather is a tool for collecting cluster data related to debugging and troubleshooting like logs, kubernetes resources, etc.",
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"node_name": {
						Type:        "string",
						Description: "Optional node to run the mustgather pod. If not provided, a random control-plane node will be selected automatically",
					},
					"node_selector": {
						Type:        "string",
						Description: "Optional node label selector to use, only relevant when specifying a command and image which needs to capture data on a set of cluster nodes simultaneously",
					},
					"host_network": {
						Type:        "boolean",
						Description: "Optionally run the must-gather pods in the host network of the node. This is only relevant if a specific gather image needs to capture host-level data",
					},
					"gather_command": {
						Type:        "string",
						Description: "Optionally specify a custom gather command to run a specialized script, eg. /usr/bin/gather_audit_logs",
						Default:     api.ToRawMessage("/usr/bin/gather"),
					},
					"all_component_images": {
						Type:        "boolean",
						Description: "Optional when enabled, collects and runs multiple must gathers for all operators and components on the cluster that have an annotated must-gather image available",
					},
					"images": {
						Type:        "array",
						Description: "Optional list of images to use for gathering custom information about specific operators or cluster components. If not specified, OpenShift's default must-gather image will be used by default",
						Items: &jsonschema.Schema{
							Type: "string",
						},
					},
					"source_dir": {
						Type:        "string",
						Description: "Optional to set a specific directory where the pod will copy gathered data from",
						Default:     api.ToRawMessage("/must-gather"),
					},
					"timeout": {
						Type:        "string",
						Description: "Timeout of the gather process eg. 30s, 6m20s, or 2h10m30s",
						Default:     api.ToRawMessage("10m"),
					},
					"namespace": {
						Type:        "string",
						Description: "Optional to specify an existing privileged namespace where must-gather pods should run. If not provided, a temporary namespace will be created",
					},
					"keep_namespace": {
						Type:        "boolean",
						Description: "Optional to retain all temporary resources when the mustgather completes, otherwise temporary resources created will be cleaned up",
					},
					"since": {
						Type:        "string",
						Description: "Optional to collect logs newer than a relative duration like 5s, 2m5s, or 3h6m10s. If unspecified, all available logs will be collected",
					},
				},
			},
			Annotations: api.ToolAnnotations{
				Title:           "MustGather: Plan",
				ReadOnlyHint:    ptr.To(true),
				DestructiveHint: ptr.To(false),
				IdempotentHint:  ptr.To(false),
				OpenWorldHint:   ptr.To(true),
			},
		},

		Handler: mustGatherPlan,
	}}
}

func mustGatherPlan(params api.ToolHandlerParams) (*api.ToolCallResult, error) {
	args := params.GetArguments()

	var nodeName, sourceDir, namespace, gatherCmd, timeout, since string
	var hostNetwork, keepNamespace, allImages bool
	var images []string
	var nodeSelector map[string]string

	if args["node_name"] != nil {
		nodeName = args["node_name"].(string)
	}

	if args["node_selector"] != nil {
		nodeSelector = parseNodeSelector(args["node_selector"].(string))
	}

	if args["host_network"] != nil {
		hostNetwork = args["host_network"].(bool)
	}

	sourceDir = defaultGatherSourceDir
	if args["source_dir"] != nil {
		sourceDir = path.Clean(args["source_dir"].(string))
	}

	namespace = fmt.Sprintf("openshift-must-gather-%s", generateRandomString(6))
	if args["namespace"] != nil {
		namespace = args["namespace"].(string)
	}

	if args["keep_namespace"] != nil {
		keepNamespace = args["keep_namespace"].(bool)
	}

	gatherCmd = defaultGatherCmd
	if args["gather_command"] != nil {
		gatherCmd = args["gather_command"].(string)
	}

	if args["all_component_images"] != nil {
		allImages = args["all_component_images"].(bool)
	}

	if args["images"] != nil {
		images = args["images"].([]string)
	}

	if args["timeout"] != nil {
		timeout = args["timeout"].(string)

		_, err := time.ParseDuration(timeout)
		if err != nil {
			return api.NewToolCallResult("", fmt.Errorf("timeout duration is not valid")), nil
		}

		gatherCmd = fmt.Sprintf("/usr/bin/timeout %s %s", timeout, gatherCmd)
	}

	if args["since"] != nil {
		since = args["since"].(string)

		_, err := time.ParseDuration(since)
		if err != nil {
			return api.NewToolCallResult("", fmt.Errorf("since duration is not valid")), nil
		}
	}

	envVars := []corev1.EnvVar{}
	if since != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "MUST_GATHER_SINCE",
			Value: since,
		})
	}

	gatherContainerTemplate := corev1.Container{
		Name:            "gather",
		Image:           defaultMustGatherImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{gatherCmd},
		Env:             envVars,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "must-gather-collection",
				MountPath: sourceDir,
			},
		},
	}

	gatherContainers := make([]corev1.Container, 1)
	gatherContainers[0] = *gatherContainerTemplate.DeepCopy()
	for i, image := range images {
		gatherContainers[i] = *gatherContainerTemplate.DeepCopy()
		gatherContainers[i].Image = image
	}

	if allImages {
		// TODO: list each ClusterOperator object and check for mgAnnotation
		// TODO: list each ClusterServiceVersion object (OLM operators) and check for mgAnnotation
		_ = allImages
		_ = mgAnnotation
	}

	serviceAccountName := "must-gather-collector"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "must-gather-",
			Namespace:    namespace,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccountName,
			NodeName:           nodeName,
			PriorityClassName:  "system-cluster-critical",
			RestartPolicy:      corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "must-gather-collection",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			Containers: append(gatherContainers, corev1.Container{
				Name:            "wait",
				Image:           "registry.redhat.io/ubi9/ubi-minimal",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/bash", "-c", "sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "must-gather-collection",
						MountPath: "/must-gather",
					},
				},
			}),
			HostNetwork:  hostNetwork,
			NodeSelector: nodeSelector,
			Tolerations: []corev1.Toleration{
				{
					Operator: "Exists",
				},
			},
		},
	}

	nsList, err := params.NamespacesList(params, internalk8s.ResourceListOptions{})
	if err != nil {
		return api.NewToolCallResult("", fmt.Errorf("failed to list namespaces: %v", err)), nil
	}

	namespaceExists := false
	if err := nsList.EachListItem(func(obj runtime.Object) error {
		if !namespaceExists {
			unstruct, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				return err
			}

			u := unstructured.Unstructured{Object: unstruct}
			if u.GetName() == namespace {
				namespaceExists = true
			}
		}

		return nil
	}); err != nil {
		return api.NewToolCallResult("", fmt.Errorf("failed to check namespaces list: %v", err)), nil
	}

	var namespaceObj *corev1.Namespace
	if !namespaceExists {
		namespaceObj = &corev1.Namespace{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Namespace",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
	}

	serviceAccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: namespace,
		},
	}

	clusterRoleBindingName := fmt.Sprintf("must-gather-collector-%s", namespace)
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleBindingName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccountName,
				Namespace: namespace,
			},
		},
	}

	var result strings.Builder
	result.WriteString("# Save the following content to a file (e.g., must-gather-plan.yaml) and apply it with 'kubectl apply -f must-gather-plan.yaml'\n")
	result.WriteString("# Monitor the pod's logs to see when the must-gather process is complete:\n")
	result.WriteString(fmt.Sprintf("# kubectl logs -f -n %s <pod-name> -c gather\n", namespace))
	result.WriteString("# Once the logs indicate completion, copy the results with:\n")
	result.WriteString(fmt.Sprintf("# kubectl cp -n %s <pod-name>:/must-gather ./must-gather-output -c wait\n", namespace))
	if !keepNamespace {
		result.WriteString("# Finally, clean up the resources with:\n")
		result.WriteString(fmt.Sprintf("# kubectl delete ns %s\n", namespace))
		result.WriteString(fmt.Sprintf("# kubectl delete clusterrolebinding %s\n", clusterRoleBindingName))
	}
	result.WriteString("\n")
	result.WriteString("```yaml")

	if !namespaceExists {
		namespaceYaml, err := yaml.Marshal(namespaceObj)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal namespace to yaml: %w", err)
		}
		result.WriteString("---\n")
		result.Write(namespaceYaml)
	}

	serviceAccountYaml, err := yaml.Marshal(serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal service account to yaml: %w", err)
	}
	result.WriteString("---\n")
	result.Write(serviceAccountYaml)

	clusterRoleBindingYaml, err := yaml.Marshal(clusterRoleBinding)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cluster role binding to yaml: %w", err)
	}
	result.WriteString("---\n")
	result.Write(clusterRoleBindingYaml)

	podYaml, err := yaml.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pod to yaml: %w", err)
	}
	result.Write(podYaml)
	result.WriteString("```")

	return api.NewToolCallResult(result.String(), nil), nil
}

func generateRandomString(length int) string {
	r := strings.ToLower(rand.Text())
	if length > len(r) {
		r = r + generateRandomString(length-len(r))
	}

	return r[:length]
}

func parseNodeSelector(selector string) map[string]string {
	result := make(map[string]string)
	pairs := strings.Split(selector, ",")
	for _, pair := range pairs {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			result[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return result
}
