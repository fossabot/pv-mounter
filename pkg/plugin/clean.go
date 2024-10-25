package plugin

import (
    "context"
    "fmt"
    "os"
    "os/exec"
    "runtime"
    "strings"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/kubernetes/scheme"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/remotecommand"
)

func Clean(ctx context.Context, namespace, pvcName, localMountPoint string) error {
    // Unmount the local mount point
    if err := unmount(localMountPoint); err != nil {
        return fmt.Errorf("failed to unmount SSHFS: %v", err)
    }
    fmt.Printf("Unmounted %s successfully\n", localMountPoint)

    // Build Kubernetes client
    clientset, config, err := BuildKubeClient()
    if err != nil {
        return err
    }

    // List the pod with the PVC name label
    podClient := clientset.CoreV1().Pods(namespace)
    podList, err := podClient.List(ctx, metav1.ListOptions{
        LabelSelector: fmt.Sprintf("pvcName=%s", pvcName),
    })
    if err != nil {
        return fmt.Errorf("failed to list pods: %v", err)
    }

    if len(podList.Items) == 0 {
        return fmt.Errorf("no pod found with PVC name label %s", pvcName)
    }

    podName := podList.Items[0].Name
    // Remove the unused variable 'port'
    // port := podList.Items[0].Labels["portNumber"]

    // Stop the port-forwarding
    // Since we're now using client-go for port-forwarding, we need to implement a way to stop it.
    // This can be managed via the stop channel in your application.

    // Check for original pod
    originalPodName := podList.Items[0].Labels["originalPodName"]
    if originalPodName != "" {
        err = killProcessInEphemeralContainer(ctx, clientset, config, namespace, originalPodName)
        if err != nil {
            return fmt.Errorf("failed to kill process in ephemeral container: %v", err)
        }
        fmt.Printf("Process in ephemeral container killed successfully in pod %s\n", originalPodName)
    }

    // Delete the proxy pod
    err = podClient.Delete(ctx, podName, metav1.DeleteOptions{})
    if err != nil {
        return fmt.Errorf("failed to delete pod: %v", err)
    }
    fmt.Printf("Proxy pod %s deleted successfully\n", podName)

    return nil
}

func killProcessInEphemeralContainer(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, namespace, podName string) error {
    // Retrieve the existing pod to get the ephemeral container name
    existingPod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get existing pod: %v", err)
    }

    if len(existingPod.Spec.EphemeralContainers) == 0 {
        return fmt.Errorf("no ephemeral containers found in pod %s", podName)
    }

    ephemeralContainerName := existingPod.Spec.EphemeralContainers[0].Name
    fmt.Printf("Ephemeral container name is %s\n", ephemeralContainerName)

    // Command to kill the process
    killCmd := []string{"pkill", "-f", "tail"} // Adjust the process name as necessary

    // Use client-go to execute the command in the ephemeral container
    req := clientset.CoreV1().RESTClient().Post().
        Resource("pods").
        Name(podName).
        Namespace(namespace).
        SubResource("exec").
        VersionedParams(&corev1.PodExecOptions{
            Container: ephemeralContainerName,
            Command:   killCmd,
            Stdin:     false,
            Stdout:    true,
            Stderr:    true,
            TTY:       false,
        }, scheme.ParameterCodec)

    exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
    if err != nil {
        return fmt.Errorf("failed to create SPDY executor: %v", err)
    }

    var stdout, stderr strings.Builder
    err = exec.Stream(remotecommand.StreamOptions{
        Stdout: &stdout,
        Stderr: &stderr,
    })
    if err != nil {
        return fmt.Errorf("failed to execute command: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
    }

    fmt.Printf("Command output:\nstdout: %s\nstderr: %s\n", stdout.String(), stderr.String())
    return nil
}

func unmount(localMountPoint string) error {
    var umountCmd *exec.Cmd
    if runtime.GOOS == "darwin" {
        umountCmd = exec.Command("umount", localMountPoint)
    } else {
        umountCmd = exec.Command("fusermount", "-u", localMountPoint)
    }
    umountCmd.Stdout = os.Stdout
    umountCmd.Stderr = os.Stderr
    if err := umountCmd.Run(); err != nil {
        return fmt.Errorf("failed to unmount %s: %v", localMountPoint, err)
    }
    return nil
}
