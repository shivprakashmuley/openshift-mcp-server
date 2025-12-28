package core

import (
	"fmt"
	"strings"

	"github.com/containers/kubernetes-mcp-server/pkg/api"
	internalk8s "github.com/containers/kubernetes-mcp-server/pkg/kubernetes"
	"github.com/google/jsonschema-go/jsonschema"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/yaml"
)

func initMustGatherPlan(o internalk8s.Openshift) []api.ServerTool {
	return []api.ServerTool{{
		Tool: api.Tool{
			Name:        "must_gather_plan",
			Description: "Provides a detailed plan (read-only) to collect a must-gather bundle based on the flags/parameters supported by oc commands.",
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"image": {
						Type:        "string",
						Description: "The image to use for the must-gather. Defaults to registry.redhat.io/openshift4/ose-must-gather:latest.",
					},
					"dest_dir": {
						Type:        "string",
						Description: "The destination directory for the output. Defaults to ./must-gather-results.",
					},
					"node_name": {
						Type:        "string",
						Description: "The node to gather information from.",
					},
					"image_stream": {
						Type:        "string",
						Description: "An image stream to use for the must-gather. (Not yet supported, use --image)",
					},
				},
			},
		},
		Handler: func(params api.ToolHandlerParams) (*api.ToolCallResult, error) {
			args := params.GetArguments()
			image, _ := args["image"].(string)
			destDir, _ := args["dest_dir"].(string)
			nodeName, _ := args["node_name"].(string)
			imageStream, _ := args["image_stream"].(string)

			if imageStream != "" {
				return nil, fmt.Errorf("the --image-stream parameter is not yet supported. Please use the --image parameter")
			}

			if image == "" {
				image = "registry.redhat.io/openshift4/ose-must-gather:latest"
			}
			if destDir == "" {
				destDir = "./must-gather-results"
			}

			suffix := rand.String(5)
			namespaceName := fmt.Sprintf("openshift-must-gather-%s", suffix)
			podName := fmt.Sprintf("must-gather-%s", suffix)
			serviceAccountName := "must-gather-admin"
			clusterRoleBindingName := fmt.Sprintf("%s-%s", namespaceName, serviceAccountName)

			namespace := &corev1.Namespace{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Namespace",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: namespaceName,
				},
			}

			serviceAccount := &corev1.ServiceAccount{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "ServiceAccount",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceAccountName,
					Namespace: namespaceName,
				},
			}

			clusterRoleBinding := &rbacv1.ClusterRoleBinding{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "ClusterRoleBinding",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: clusterRoleBindingName,
				},
				Subjects: []rbacv1.Subject{
					{
						Kind:      "ServiceAccount",
						Name:      serviceAccountName,
						Namespace: namespaceName,
					},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io",
					Kind:     "ClusterRole",
					Name:     "cluster-admin",
				},
			}

			pod := &corev1.Pod{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "v1",
					Kind:       "Pod",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: namespaceName,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "must-gather",
							Image: image,
							Command: []string{
								"/bin/sh",
								"-c",
								"/usr/bin/gather && sleep infinity",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "must-gather-output",
									MountPath: "/must-gather",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "must-gather-output",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					RestartPolicy:      corev1.RestartPolicyNever,
					NodeName:           nodeName,
					ServiceAccountName: serviceAccountName,
				},
			}

			namespaceYaml, err := yaml.Marshal(namespace)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal namespace to yaml: %w", err)
			}
			serviceAccountYaml, err := yaml.Marshal(serviceAccount)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal service account to yaml: %w", err)
			}
			clusterRoleBindingYaml, err := yaml.Marshal(clusterRoleBinding)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal cluster role binding to yaml: %w", err)
			}
			podYaml, err := yaml.Marshal(pod)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal pod to yaml: %w", err)
			}

			var result strings.Builder
			result.WriteString("# Save the following content to a file (e.g., must-gather-plan.yaml) and apply it with 'kubectl apply -f must-gather-plan.yaml'\n")
			result.WriteString("# Monitor the pod's logs to see when the must-gather process is complete:\n")
			result.WriteString(fmt.Sprintf("# kubectl logs -f -n %s %s\n", namespaceName, podName))
			result.WriteString("# Once the logs indicate completion, copy the results with:\n")
			result.WriteString(fmt.Sprintf("# kubectl cp -n %s %s:/must-gather %s\n", namespaceName, podName, destDir))
			result.WriteString("# Finally, clean up the resources with:\n")
			result.WriteString(fmt.Sprintf("# kubectl delete ns %s\n", namespaceName))
			result.WriteString(fmt.Sprintf("# kubectl delete clusterrolebinding %s\n", clusterRoleBindingName))
			result.WriteString("---\n")
			result.Write(namespaceYaml)
			result.WriteString("---\n")
			result.Write(serviceAccountYaml)
			result.WriteString("---\n")
			result.Write(clusterRoleBindingYaml)
			result.WriteString("---\n")
			result.Write(podYaml)

			return api.NewToolCallResult(result.String(), nil), nil
		},
	}}
}
