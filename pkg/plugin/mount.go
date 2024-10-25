package plugin

import (
    "context"
    "crypto/elliptic"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "math/rand"
    "net/url"
    "os"
    "os/exec"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/resource"
    "k8s.io/apimachinery/pkg/types"
    "k8s.io/apimachinery/pkg/util/httpstream"
    "k8s.io/apimachinery/pkg/util/wait"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    portforward "k8s.io/client-go/tools/portforward"
    "k8s.io/client-go/transport/spdy"
)

const (
    ImageVersion = "v0.2.3"
    Image                  = "bfenski/volume-exposer:" + ImageVersion
    PrivilegedImage        = "bfenski/volume-exposer-privileged:" + ImageVersion
    DefaultUserGroup int64 = 2137
    DefaultSSHPort   int   = 2137
    ProxySSHPort     int   = 6666

    CPURequest              = "10m"
    MemoryRequest           = "50Mi"
    MemoryLimit             = "100Mi"
    EphemeralStorageRequest = "1Mi"
    EphemeralStorageLimit   = "2Mi"
)

func Mount(ctx context.Context, namespace, pvcName, localMountPoint string, needsRoot, debug bool) error {
    checkSSHFS()

    if err := validateMountPoint(localMountPoint); err != nil {
        return err
    }

    clientset, config, err := BuildKubeClient()
    if err != nil {
        return err
    }

    pvc, err := checkPVCUsage(ctx, clientset, namespace, pvcName)
    if err != nil {
        return err
    }

    canBeMounted, podUsingPVC, err := checkPVAccessMode(ctx, clientset, pvc, namespace)
    if err != nil {
        return err
    }

    // Generate the key pair once and use it for both standalone and proxy scenarios
    privateKey, publicKey, err := GenerateKeyPair(elliptic.P256())
    if err != nil {
        return fmt.Errorf("error generating key pair: %v", err)
    }

    if debug {
        fmt.Printf("Debug mode enabled\n")
    }

    if canBeMounted {
        return handleRWX(ctx, clientset, config, namespace, pvcName, localMountPoint, privateKey, publicKey, needsRoot)
    } else {
        return handleRWO(ctx, clientset, config, namespace, pvcName, localMountPoint, podUsingPVC, privateKey, publicKey, needsRoot)
    }
}

func validateMountPoint(localMountPoint string) error {
    if _, err := os.Stat(localMountPoint); os.IsNotExist(err) {
        return fmt.Errorf("local mount point %s does not exist", localMountPoint)
    }
    return nil
}

func handleRWX(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, namespace, pvcName, localMountPoint, privateKey, publicKey string, needsRoot bool) error {
    podName, port, err := setupPod(ctx, clientset, namespace, pvcName, publicKey, "standalone", DefaultSSHPort, "", needsRoot)
    if err != nil {
        return err
    }

    if err := waitForPodReady(ctx, clientset, namespace, podName); err != nil {
        return err
    }

    stopCh := make(chan struct{}, 1)
    defer close(stopCh)

    readyCh := make(chan struct{})
    defer close(readyCh)

    // Set up port forwarding
    pf, err := setupPortForwarding(ctx, config, namespace, podName, port, DefaultSSHPort, stopCh, readyCh)
    if err != nil {
        return err
    }

    // Wait for port forwarding to be ready
    select {
    case <-readyCh:
        fmt.Println("Port forwarding is ready")
    case <-time.After(10 * time.Second):
        return fmt.Errorf("timeout waiting for port forwarding to be ready")
    }

    return mountPVCOverSSH(namespace, podName, port, localMountPoint, pvcName, privateKey, needsRoot)
}

func handleRWO(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, namespace, pvcName, localMountPoint, podUsingPVC, privateKey, publicKey string, needsRoot bool) error {
    podName, port, err := setupPod(ctx, clientset, namespace, pvcName, publicKey, "proxy", ProxySSHPort, podUsingPVC, needsRoot)
    if err != nil {
        return err
    }

    if err := waitForPodReady(ctx, clientset, namespace, podName); err != nil {
        return err
    }

    proxyPodIP, err := getPodIP(ctx, clientset, namespace, podName)
    if err != nil {
        return err
    }

    if err := createEphemeralContainer(ctx, clientset, namespace, podUsingPVC, privateKey, publicKey, proxyPodIP, needsRoot); err != nil {
        return err
    }

    stopCh := make(chan struct{}, 1)
    defer close(stopCh)

    readyCh := make(chan struct{})
    defer close(readyCh)

    // Set up port forwarding
    pf, err := setupPortForwarding(ctx, config, namespace, podName, port, DefaultSSHPort, stopCh, readyCh)
    if err != nil {
        return err
    }

    // Wait for port forwarding to be ready
    select {
    case <-readyCh:
        fmt.Println("Port forwarding is ready")
    case <-time.After(10 * time.Second):
        return fmt.Errorf("timeout waiting for port forwarding to be ready")
    }

    return mountPVCOverSSH(namespace, podName, port, localMountPoint, pvcName, privateKey, needsRoot)
}

func createEphemeralContainer(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName, privateKey, publicKey, proxyPodIP string, needsRoot bool) error {
    // Retrieve the existing pod to get the volume name
    existingPod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
    if err != nil {
        return fmt.Errorf("failed to get existing pod: %v", err)
    }

    volumeName, err := getPVCVolumeName(existingPod)
    if err != nil {
        return err
    }

    ephemeralContainerName := fmt.Sprintf("volume-exposer-ephemeral-%s", randSeq(5))
    fmt.Printf("Adding ephemeral container %s to pod %s with volume name %s\n", ephemeralContainerName, podName, volumeName)

    image, securityContext := getEphemeralContainerSettings(needsRoot)

    ephemeralContainer := corev1.EphemeralContainer{
        EphemeralContainerCommon: corev1.EphemeralContainerCommon{
            Name:            ephemeralContainerName,
            Image:           image,
            ImagePullPolicy: corev1.PullAlways,
            Env: []corev1.EnvVar{
                {Name: "ROLE", Value: "ephemeral"},
                {Name: "SSH_PRIVATE_KEY", Value: privateKey},
                {Name: "PROXY_POD_IP", Value: proxyPodIP},
                {Name: "SSH_PUBLIC_KEY", Value: publicKey},
                {Name: "NEEDS_ROOT", Value: fmt.Sprintf("%v", needsRoot)},
            },
            SecurityContext: securityContext,
            VolumeMounts: []corev1.VolumeMount{
                {
                    Name:      volumeName,
                    MountPath: "/volume",
                },
            },
        },
    }

    // Patch the pod to add the ephemeral container
    patchData, err := json.Marshal(map[string]interface{}{
        "spec": map[string]interface{}{
            "ephemeralContainers": append(existingPod.Spec.EphemeralContainers, ephemeralContainer),
        },
    })
    if err != nil {
        return fmt.Errorf("failed to marshal ephemeral container spec: %v", err)
    }

    _, err = clientset.CoreV1().Pods(namespace).Patch(ctx, podName, types.StrategicMergePatchType, patchData, metav1.PatchOptions{}, "ephemeralcontainers")
    if err != nil {
        return fmt.Errorf("failed to patch pod with ephemeral container: %v", err)
    }

    fmt.Printf("Successfully added ephemeral container %s to pod %s\n", ephemeralContainerName, podName)
    return nil
}

func getPodIP(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) (string, error) {
    pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
    if err != nil {
        return "", fmt.Errorf("failed to get pod IP: %v", err)
    }
    return pod.Status.PodIP, nil
}

func checkPVAccessMode(ctx context.Context, clientset *kubernetes.Clientset, pvc *corev1.PersistentVolumeClaim, namespace string) (bool, string, error) {
    pvName := pvc.Spec.VolumeName
    pv, err := clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
    if err != nil {
        return true, "", fmt.Errorf("failed to get PV: %v", err)
    }

    if contains(pv.Spec.AccessModes, corev1.ReadWriteOnce) {
        podList, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            return true, "", fmt.Errorf("failed to list pods: %v", err)
        }
        for _, pod := range podList.Items {
            for _, volume := range pod.Spec.Volumes {
                if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
                    return false, pod.Name, nil
                }
            }
        }
    }
    return true, "", nil
}

func contains(modes []corev1.PersistentVolumeAccessMode, modeToFind corev1.PersistentVolumeAccessMode) bool {
    for _, mode := range modes {
        if mode == modeToFind {
            return true
        }
    }
    return false
}

func checkPVCUsage(ctx context.Context, clientset *kubernetes.Clientset, namespace, pvcName string) (*corev1.PersistentVolumeClaim, error) {
    pvc, err := clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
    if err != nil {
        return nil, fmt.Errorf("failed to get PVC: %v", err)
    }
    if pvc.Status.Phase != corev1.ClaimBound {
        return nil, fmt.Errorf("PVC %s is not bound", pvcName)
    }
    return pvc, nil
}

func setupPod(ctx context.Context, clientset *kubernetes.Clientset, namespace, pvcName, publicKey, role string, sshPort int, originalPodName string, needsRoot bool) (string, int, error) {
    podName, port := generatePodNameAndPort(pvcName, role)
    pod := createPodSpec(podName, port, pvcName, publicKey, role, sshPort, originalPodName, needsRoot)
    if _, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
        return "", 0, fmt.Errorf("failed to create pod: %v", err)
    }
    fmt.Printf("Pod %s created successfully\n", podName)
    return podName, port, nil
}

func waitForPodReady(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) error {
    return wait.PollImmediate(time.Second, 5*time.Minute, func() (bool, error) {
        pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
        if err != nil {
            return false, err
        }
        for _, cond := range pod.Status.Conditions {
            if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
                return true, nil
            }
        }
        return false, nil
    })
}

func setupPortForwarding(ctx context.Context, config *rest.Config, namespace, podName string, localPort, podPort int, stopCh, readyCh chan struct{}) (*portforward.PortForwarder, error) {
    // Create a roundtripper
    path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, podName)
    hostIP := strings.TrimLeft(config.Host, "htps:/")

    url := url.URL{Scheme: "https", Path: path, Host: hostIP}

    transport, upgrader, err := spdy.RoundTripperFor(config)
    if err != nil {
        return nil, fmt.Errorf("failed to create round tripper: %v", err)
    }

    dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", &url)

    ports := []string{fmt.Sprintf("%d:%d", localPort, podPort)}
    pf, err := portforward.New(dialer, ports, stopCh, readyCh, os.Stdout, os.Stderr)
    if err != nil {
        return nil, fmt.Errorf("failed to create port forwarder: %v", err)
    }

    // Start port forwarding in a goroutine
    go func() {
        if err := pf.ForwardPorts(); err != nil {
            fmt.Fprintf(os.Stderr, "Error in port forwarding: %v\n", err)
        }
    }()

    return pf, nil
}

func mountPVCOverSSH(
    namespace, podName string,
    port int,
    localMountPoint, pvcName, privateKey string,
    needsRoot bool) error {

    // Create a temporary file to store the private key
    tmpFile, err := ioutil.TempFile("", "ssh_key_*.pem")
    if err != nil {
        return fmt.Errorf("failed to create temporary file for SSH private key: %v", err)
    }
    defer func() {
        tmpFile.Close()
        os.Remove(tmpFile.Name())
    }()

    if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
        return fmt.Errorf("failed to set permissions on temporary file: %v", err)
    }

    if _, err := tmpFile.Write([]byte(privateKey)); err != nil {
        return fmt.Errorf("failed to write SSH private key to temporary file: %v", err)
    }
    if err := tmpFile.Close(); err != nil {
        return fmt.Errorf("failed to close temporary file: %v", err)
    }

    sshUser := "ve"
    if needsRoot {
        sshUser = "root"
    }

    sshfsCmd := exec.Command(
        "sshfs",
        "-o", fmt.Sprintf("IdentityFile=%s", tmpFile.Name()),
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        fmt.Sprintf("%s@localhost:/volume", sshUser),
        localMountPoint,
        "-p", fmt.Sprintf("%d", port),
    )

    sshfsCmd.Stdout = os.Stdout
    sshfsCmd.Stderr = os.Stderr

    if err := sshfsCmd.Run(); err != nil {
        return fmt.Errorf("failed to mount PVC using SSHFS: %v", err)
    }

    fmt.Printf("PVC %s mounted successfully to %s\n", pvcName, localMountPoint)
    return nil
}

func generatePodNameAndPort(pvcName, role string) (string, int) {
    rand.Seed(time.Now().UnixNano())
    suffix := randSeq(5)
    baseName := "volume-exposer"
    if role == "proxy" {
        baseName = "volume-exposer-proxy"
    }
    podName := fmt.Sprintf("%s-%s", baseName, suffix)
    port := rand.Intn(64511) + 1024 // Generate a random port between 1024 and 65535
    return podName, port
}

func createPodSpec(podName string, port int, pvcName, publicKey, role string, sshPort int, originalPodName string, needsRoot bool) *corev1.Pod {

    envVars := []corev1.EnvVar{
        {Name: "SSH_PUBLIC_KEY", Value: publicKey},
        {Name: "SSH_PORT", Value: fmt.Sprintf("%d", sshPort)},
        {Name: "NEEDS_ROOT", Value: fmt.Sprintf("%v", needsRoot)},
    }

    // Add the ROLE environment variable if the role is "standalone" or "proxy"
    if role == "standalone" || role == "proxy" {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "ROLE",
            Value: role,
        })
    }

    image, securityContext := getEphemeralContainerSettings(needsRoot)

    runAsNonRoot := !needsRoot
    runAsUser := int64(DefaultUserGroup)
    runAsGroup := int64(DefaultUserGroup)
    if needsRoot {
        runAsUser = 0
        runAsGroup = 0
    }

    container := corev1.Container{
        Name:            "volume-exposer",
        Image:           image,
        ImagePullPolicy: corev1.PullAlways,
        Ports: []corev1.ContainerPort{
            {ContainerPort: int32(sshPort)},
        },
        Env:             envVars,
        SecurityContext: securityContext,
        Resources: corev1.ResourceRequirements{
            Requests: corev1.ResourceList{
                corev1.ResourceCPU:              resource.MustParse(CPURequest),
                corev1.ResourceMemory:           resource.MustParse(MemoryRequest),
                corev1.ResourceEphemeralStorage: resource.MustParse(EphemeralStorageRequest),
            },
            Limits: corev1.ResourceList{
                corev1.ResourceMemory:           resource.MustParse(MemoryLimit),
                corev1.ResourceEphemeralStorage: resource.MustParse(EphemeralStorageLimit),
            },
        },
    }

    labels := map[string]string{
        "app":        "volume-exposer",
        "pvcName":    pvcName,
        "portNumber": fmt.Sprintf("%d", port),
    }

    // Add the original pod name label if provided
    if originalPodName != "" {
        labels["originalPodName"] = originalPodName
    }

    podSpec := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:   podName,
            Labels: labels,
        },
        Spec: corev1.PodSpec{
            Containers: []corev1.Container{container},
            SecurityContext: &corev1.PodSecurityContext{
                RunAsNonRoot: &runAsNonRoot,
                RunAsUser:    &runAsUser,
                RunAsGroup:   &runAsGroup,
            },
        },
    }

    // Only mount the volume if the role is not "proxy"
    if role != "proxy" {
        container.VolumeMounts = []corev1.VolumeMount{
            {MountPath: "/volume", Name: "my-pvc"},
        }
        podSpec.Spec.Volumes = []corev1.Volume{
            {
                Name: "my-pvc",
                VolumeSource: corev1.VolumeSource{
                    PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
                        ClaimName: pvcName,
                    },
                },
            },
        }
        // Update the container in the podSpec with the volume mounts
        podSpec.Spec.Containers[0] = container
    }

    return podSpec
}

func getPVCVolumeName(pod *corev1.Pod) (string, error) {
    for _, volume := range pod.Spec.Volumes {
        if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
            return volume.Name, nil
        }
    }
    return "", fmt.Errorf("failed to find volume name in the existing pod")
}

func getEphemeralContainerSettings(needsRoot bool) (string, *corev1.SecurityContext) {
    image := Image
    var securityContext *corev1.SecurityContext

    // Define boolean pointers inline
    allowPrivilegeEscalationTrue := true
    allowPrivilegeEscalationFalse := false
    readOnlyRootFilesystemTrue := true

    if needsRoot {
        image = PrivilegedImage
        securityContext = &corev1.SecurityContext{
            AllowPrivilegeEscalation: &allowPrivilegeEscalationTrue,
            ReadOnlyRootFilesystem:   &readOnlyRootFilesystemTrue,
            Capabilities: &corev1.Capabilities{
                Add: []corev1.Capability{"SYS_ADMIN", "SYS_CHROOT"},
            },
        }
    } else {
        securityContext = &corev1.SecurityContext{
            AllowPrivilegeEscalation: &allowPrivilegeEscalationFalse,
            ReadOnlyRootFilesystem:   &readOnlyRootFilesystemTrue,
            Capabilities: &corev1.Capabilities{
                Drop: []corev1.Capability{"ALL"},
            },
        }
    }
    return image, securityContext
}
