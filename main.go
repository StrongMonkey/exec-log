package main

import (
	"net/http"
	"os"

	"strconv"

	"fmt"
	"io"

	"time"

	dockerterm "github.com/docker/docker/pkg/term"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubernetes/pkg/api"
	kubeutil "k8s.io/kubernetes/pkg/kubectl/util"
	"k8s.io/kubernetes/pkg/kubectl/util/term"
)

type manager struct {
	coreClient *kubernetes.Clientset
	restClient *rest.RESTClient
	config     *rest.Config
}

type ExecOption struct {
	podName       string
	nameSpace     string
	containerName string
	stdin         bool
	stdout        bool
	stderr        bool
	tty           bool
	command       []string
}

func (m *manager) HandleExec(w http.ResponseWriter, r *http.Request) {
	options := getExecOption(r)
	pod, err := m.coreClient.CoreV1().Pods(options.nameSpace).Get(options.podName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		http.Error(w, fmt.Sprintf("cannot exec into a container in a completed pod; current phase is %s", pod.Status.Phase), http.StatusInternalServerError)
	}

	containerName := options.containerName
	if len(containerName) == 0 {
		containerName = pod.Spec.Containers[0].Name
	}
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	ioWrapper := &WebsocketIOWrapper{
		Conn: conn,
	}

	steamOptions := StreamOptions{
		Namespace:     options.nameSpace,
		PodName:       options.podName,
		ContainerName: options.containerName,
		Stdin:         true,
		TTY:           options.tty,
		In:            ioWrapper,
		Out:           ioWrapper,
		Err:           ioWrapper,
	}
	t := steamOptions.setupTTY()
	var sizeQueue remotecommand.TerminalSizeQueue
	if t.Raw {
		// this call spawns a goroutine to monitor/update the terminal size
		sizeQueue = t.MonitorSize(t.GetSize())

		// unset p.Err if it was previously set because both stdout and stderr go over p.Out when tty is
		// true
		steamOptions.Err = nil
	}

	fn := func() error {
		req := m.restClient.Post().
			Resource("pods").
			Name(options.podName).
			Namespace(options.nameSpace).
			SubResource("exec")

		execOption := &v1.PodExecOptions{
			Container: containerName,
			Command:   options.command,
			Stdin:     options.stdin,
			Stdout:    options.stdout,
			Stderr:    options.stderr,
			TTY:       options.tty,
		}
		req.VersionedParams(execOption, dynamic.VersionedParameterEncoderWithV1Fallback)
		executor, err := remotecommand.NewSPDYExecutor(m.config, "POST", req.URL())
		if err != nil {
			conn.WriteControl(websocket.CloseMessage, []byte(err.Error()), time.Now().Add(time.Second*30))
		}
		option := remotecommand.StreamOptions{
			Stdin:             ioWrapper,
			Stdout:            ioWrapper,
			Stderr:            ioWrapper,
			Tty:               t.Raw,
			TerminalSizeQueue: sizeQueue,
		}
		if option.Tty {
			option.Stderr = nil
		}
		return executor.Stream(option)
	}

	if err := t.Safe(fn); err != nil {
		conn.WriteControl(websocket.CloseMessage, []byte(err.Error()), time.Now().Add(time.Second*30))
	}
}

type WebsocketIOWrapper struct {
	Conn *websocket.Conn
}

func (w *WebsocketIOWrapper) Read(p []byte) (n int, err error) {
	_, data, err := w.Conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	return copy(p, data), nil
}

func (w *WebsocketIOWrapper) Write(p []byte) (n int, err error) {
	return len(p), w.Conn.WriteMessage(websocket.TextMessage, p)
}

type StreamOptions struct {
	Namespace     string
	PodName       string
	ContainerName string
	Stdin         bool
	TTY           bool
	// minimize unnecessary output
	Quiet bool
	In    io.Reader
	Out   io.Writer
	Err   io.Writer
}

func (o *StreamOptions) setupTTY() term.TTY {
	t := term.TTY{
		Out: o.Out,
	}

	if !o.Stdin {
		// need to nil out o.In to make sure we don't create a stream for stdin
		o.In = nil
		o.TTY = false
		return t
	}

	t.In = o.In
	if !o.TTY {
		return t
	}

	// if we get to here, the user wants to attach stdin, wants a TTY, and o.In is a terminal, so we
	// can safely set t.Raw to true
	t.Raw = true

	stdin, stdout, _ := dockerterm.StdStreams()
	o.In = stdin
	t.In = stdin
	if o.Out != nil {
		o.Out = stdout
		t.Out = stdout
	}

	return t
}

func getExecOption(r *http.Request) ExecOption {
	values := r.URL.Query()
	option := ExecOption{}
	for k, v := range values {
		switch k {
		case "container":
			option.containerName = v[0]
		case "command":
			option.command = v
		case "namespace":
			option.nameSpace = v[0]
		case "pod":
			option.podName = v[0]
		case "tty":
			option.tty, _ = strconv.ParseBool(v[0])
		case "stdin":
			option.stdin, _ = strconv.ParseBool(v[0])
		case "stdout":
			option.stdout, _ = strconv.ParseBool(v[0])
		case "stderr":
			option.stderr, _ = strconv.ParseBool(v[0])
		}
	}
	return option
}

func (m *manager) HandleLog(w http.ResponseWriter, r *http.Request) {
	options, err := generateLogOptions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	pod, err := m.coreClient.CoreV1().Pods(options.NameSpace).Get(options.PodName, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	containerName := options.Container
	if len(containerName) == 0 {
		containerName = pod.Spec.Containers[0].Name
	}
	conn, err := websocket.Upgrade(w, r, w.Header(), 1024, 1024)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	ioWrapper := &WebsocketIOWrapper{
		Conn: conn,
	}
	podLogOption := v1.PodLogOptions{
		Container:  containerName,
		Timestamps: options.Timestamps,
		Follow:     options.Follow,
		SinceTime:  options.SinceTime,
	}
	req := m.restClient.Get().
		Resource("pods").
		Name(options.PodName).
		Namespace(options.NameSpace).
		SubResource("log").
		VersionedParams(&podLogOption, dynamic.VersionedParameterEncoderWithV1Fallback)

	reader, err := req.Stream()
	if err != nil {
		conn.WriteControl(websocket.CloseMessage, []byte(err.Error()), time.Now().Add(time.Second*30))
		return
	}
	defer reader.Close()
	_, err = io.Copy(ioWrapper, reader)
	if err != nil {
		conn.WriteControl(websocket.CloseMessage, []byte(err.Error()), time.Now().Add(time.Second*30))
	}
}

type LogOptions struct {
	api.PodLogOptions
	PodName   string
	NameSpace string
}

func generateLogOptions(r *http.Request) (LogOptions, error) {
	values := r.URL.Query()
	options := LogOptions{}
	for k, v := range values {
		switch k {
		case "pod":
			options.PodName = v[0]
		case "namespace":
			options.NameSpace = v[0]
		case "containerName":
			options.Container = v[0]
		case "timestamp":
			options.Timestamps, _ = strconv.ParseBool(v[0])
		case "follow":
			options.Follow, _ = strconv.ParseBool(v[0])
		case "since":
			t, err := kubeutil.ParseRFC3339(v[0], metav1.Now)
			if err != nil {
				return options, err
			}
			options.SinceTime = &t
		}
	}
	return options, nil
}

func main() {
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		logrus.Fatal(err)
	}
	gv := v1.SchemeGroupVersion
	contentConfig := rest.ContentConfig{}
	contentConfig.GroupVersion = &gv
	contentConfig.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: scheme.Codecs}
	cfg.APIPath = "/api"
	cfg.ContentConfig = contentConfig
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logrus.Fatal(err)
	}
	restClient, err := rest.RESTClientFor(cfg)
	if err != nil {
		logrus.Fatal(err)
	}
	m := manager{
		restClient: restClient,
		coreClient: clientset,
		config:     cfg,
	}
	http.HandleFunc("/exec", m.HandleExec)
	http.HandleFunc("/log", m.HandleLog)
	err = http.ListenAndServe(fmt.Sprintf(":%d", 9001), nil)
	if err != nil {
		logrus.Fatal(err)
	}
}
