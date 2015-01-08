/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubelet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/latest"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/httplog"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/types"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/httpstream"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/httpstream/spdy"
	"github.com/golang/glog"
	"github.com/google/cadvisor/info"
)

// Server is a http.Handler which exposes kubelet functionality over HTTP.
type Server struct {
	host HostInterface
	mux  *http.ServeMux
}

// ListenAndServeKubeletServer initializes a server to respond to HTTP network requests on the Kubelet.
func ListenAndServeKubeletServer(host HostInterface, address net.IP, port uint, enableDebuggingHandlers bool) {
	glog.V(1).Infof("Starting to listen on %s:%d", address, port)
	handler := NewServer(host, enableDebuggingHandlers)
	s := &http.Server{
		Addr:           net.JoinHostPort(address.String(), strconv.FormatUint(uint64(port), 10)),
		Handler:        &handler,
		ReadTimeout:    5 * time.Minute,
		WriteTimeout:   5 * time.Minute,
		MaxHeaderBytes: 1 << 20,
	}
	glog.Fatal(s.ListenAndServe())
}

// HostInterface contains all the kubelet methods required by the server.
// For testablitiy.
type HostInterface interface {
	GetContainerInfo(podFullName string, uid types.UID, containerName string, req *info.ContainerInfoRequest) (*info.ContainerInfo, error)
	GetRootInfo(req *info.ContainerInfoRequest) (*info.ContainerInfo, error)
	GetDockerVersion() ([]uint, error)
	GetMachineInfo() (*info.MachineInfo, error)
	GetBoundPods() ([]api.BoundPod, error)
	GetPodByName(namespace, name string) (*api.BoundPod, bool)
	GetPodStatus(name string, uid types.UID) (api.PodStatus, error)
	RunInContainer(name string, uid types.UID, container string, cmd []string) ([]byte, error)
	ExecInContainer(name string, uid types.UID, container string, cmd []string, in io.Reader, out, err io.WriteCloser, tty bool) error
	GetKubeletContainerLogs(podFullName, containerName, tail string, follow bool, stdout, stderr io.Writer) error
	ServeLogs(w http.ResponseWriter, req *http.Request)
	PortForward(name string, uid types.UID, port uint16, stream io.ReadWriteCloser) error
	StreamingConnectionIdleTimeout() time.Duration
}

// NewServer initializes and configures a kubelet.Server object to handle HTTP requests.
func NewServer(host HostInterface, enableDebuggingHandlers bool) Server {
	server := Server{
		host: host,
		mux:  http.NewServeMux(),
	}
	server.InstallDefaultHandlers()
	if enableDebuggingHandlers {
		server.InstallDebuggingHandlers()
	}
	return server
}

// InstallDefaultHandlers registers the default set of supported HTTP request patterns with the mux.
func (s *Server) InstallDefaultHandlers() {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/podInfo", s.handlePodInfoOld)
	s.mux.HandleFunc("/api/v1beta1/podInfo", s.handlePodInfoVersioned)
	s.mux.HandleFunc("/boundPods", s.handleBoundPods)
	s.mux.HandleFunc("/stats/", s.handleStats)
	s.mux.HandleFunc("/spec/", s.handleSpec)
}

// InstallDeguggingHandlers registers the HTTP request patterns that serve logs or run commands/containers
func (s *Server) InstallDebuggingHandlers() {
	s.mux.HandleFunc("/run/", s.handleRun)
	s.mux.HandleFunc("/exec/", s.handleExec)
	s.mux.HandleFunc("/portForward/", s.handlePortForward)

	s.mux.HandleFunc("/logs/", s.handleLogs)
	s.mux.HandleFunc("/containerLogs/", s.handleContainerLogs)
}

// error serializes an error object into an HTTP response.
func (s *Server) error(w http.ResponseWriter, err error) {
	http.Error(w, fmt.Sprintf("Internal Error: %v", err), http.StatusInternalServerError)
}

func isValidDockerVersion(ver []uint) (bool, string) {
	minAllowedVersion := []uint{1, 15}
	for i := 0; i < len(ver) && i < len(minAllowedVersion); i++ {
		if ver[i] != minAllowedVersion[i] {
			if ver[i] < minAllowedVersion[i] {
				versions := make([]string, len(ver))
				for i, v := range ver {
					versions[i] = fmt.Sprint(v)
				}
				return false, strings.Join(versions, ".")
			}
			return true, ""
		}
	}
	return true, ""
}

// handleHealthz handles /healthz request and checks Docker version
func (s *Server) handleHealthz(w http.ResponseWriter, req *http.Request) {
	versions, err := s.host.GetDockerVersion()
	if err != nil {
		s.error(w, errors.New("unknown Docker version"))
		return
	}
	valid, version := isValidDockerVersion(versions)
	if !valid {
		s.error(w, errors.New("Docker version is too old ("+version+")"))
		return
	}
	w.Write([]byte("ok"))
}

// handleContainerLogs handles containerLogs request against the Kubelet
func (s *Server) handleContainerLogs(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	u, err := url.ParseRequestURI(req.RequestURI)
	if err != nil {
		s.error(w, err)
		return
	}
	parts := strings.Split(u.Path, "/")

	// req URI: /containerLogs/<podNamespace>/<podID>/<containerName>
	var podNamespace, podID, containerName string
	if len(parts) == 5 {
		podNamespace = parts[2]
		podID = parts[3]
		containerName = parts[4]
	} else {
		http.Error(w, "Unexpected path for command running", http.StatusBadRequest)
		return
	}

	if len(podID) == 0 {
		http.Error(w, `{"message": "Missing podID."}`, http.StatusBadRequest)
		return
	}
	if len(containerName) == 0 {
		http.Error(w, `{"message": "Missing container name."}`, http.StatusBadRequest)
		return
	}
	if len(podNamespace) == 0 {
		http.Error(w, `{"message": "Missing podNamespace."}`, http.StatusBadRequest)
		return
	}

	uriValues := u.Query()
	follow, _ := strconv.ParseBool(uriValues.Get("follow"))
	tail := uriValues.Get("tail")

	pod, ok := s.host.GetPodByName(podNamespace, podID)
	if !ok {
		http.Error(w, fmt.Sprintf("Pod %q does not exist", podID), http.StatusNotFound)
		return
	}
	// Check if containerName is valid.
	containerExists := false
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			containerExists = true
		}
	}
	if !containerExists {
		http.Error(w, fmt.Sprintf("Container %q not found in Pod %q", containerName, podID), http.StatusNotFound)
		return
	}

	fw := FlushWriter{writer: w}
	if flusher, ok := fw.writer.(http.Flusher); ok {
		fw.flusher = flusher
	} else {
		s.error(w, fmt.Errorf("unable to convert %v into http.Flusher", fw))
	}
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	err = s.host.GetKubeletContainerLogs(GetPodFullName(pod), containerName, tail, follow, &fw, &fw)
	if err != nil {
		s.error(w, err)
		return
	}
}

// handleBoundPods returns a list of pod bound to the Kubelet and their spec
func (s *Server) handleBoundPods(w http.ResponseWriter, req *http.Request) {
	pods, err := s.host.GetBoundPods()
	if err != nil {
		s.error(w, err)
		return
	}
	boundPods := &api.BoundPods{
		Items: pods,
	}
	data, err := latest.Codec.Encode(boundPods)
	if err != nil {
		s.error(w, err)
		return
	}
	w.Header().Add("Content-type", "application/json")
	w.Write(data)
}

func (s *Server) handlePodInfoOld(w http.ResponseWriter, req *http.Request) {
	s.handlePodStatus(w, req, false)
}

func (s *Server) handlePodInfoVersioned(w http.ResponseWriter, req *http.Request) {
	s.handlePodStatus(w, req, true)
}

// handlePodStatus handles podInfo requests against the Kubelet
func (s *Server) handlePodStatus(w http.ResponseWriter, req *http.Request, versioned bool) {
	u, err := url.ParseRequestURI(req.RequestURI)
	if err != nil {
		s.error(w, err)
		return
	}
	podID := u.Query().Get("podID")
	podUID := types.UID(u.Query().Get("UUID"))
	podNamespace := u.Query().Get("podNamespace")
	if len(podID) == 0 {
		http.Error(w, "Missing 'podID=' query entry.", http.StatusBadRequest)
		return
	}
	if len(podNamespace) == 0 {
		http.Error(w, "Missing 'podNamespace=' query entry.", http.StatusBadRequest)
		return
	}
	pod, ok := s.host.GetPodByName(podNamespace, podID)
	if !ok {
		http.Error(w, "Pod does not exist", http.StatusNotFound)
		return
	}
	status, err := s.host.GetPodStatus(GetPodFullName(pod), podUID)
	if err != nil {
		s.error(w, err)
		return
	}
	data, err := exportPodStatus(status, versioned)
	if err != nil {
		s.error(w, err)
		return
	}
	w.Header().Add("Content-type", "application/json")
	w.Write(data)
}

// handleStats handles stats requests against the Kubelet.
func (s *Server) handleStats(w http.ResponseWriter, req *http.Request) {
	s.serveStats(w, req)
}

// handleLogs handles logs requests against the Kubelet.
func (s *Server) handleLogs(w http.ResponseWriter, req *http.Request) {
	s.host.ServeLogs(w, req)
}

// handleSpec handles spec requests against the Kubelet.
func (s *Server) handleSpec(w http.ResponseWriter, req *http.Request) {
	info, err := s.host.GetMachineInfo()
	if err != nil {
		s.error(w, err)
		return
	}
	data, err := json.Marshal(info)
	if err != nil {
		s.error(w, err)
		return
	}
	w.Header().Add("Content-type", "application/json")
	w.Write(data)
}

func parseContainerCoordinates(path string) (namespace, pod string, uid types.UID, container string, err error) {
	parts := strings.Split(path, "/")

	if len(parts) == 5 {
		namespace = parts[2]
		pod = parts[3]
		container = parts[4]
		return
	}

	if len(parts) == 6 {
		namespace = parts[2]
		pod = parts[3]
		uid = types.UID(parts[4])
		container = parts[5]
		return
	}

	err = fmt.Errorf("Unexpected path %s. Expected /.../.../<namespace>/<pod>/<container> or /.../.../<namespace>/<pod>/<uid>/<container>", path)
	return
}

// handleRun handles requests to run a command inside a container.
func (s *Server) handleRun(w http.ResponseWriter, req *http.Request) {
	u, err := url.ParseRequestURI(req.RequestURI)
	if err != nil {
		s.error(w, err)
		return
	}
	podNamespace, podID, uid, container, err := parseContainerCoordinates(u.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pod, ok := s.host.GetPodByName(podNamespace, podID)
	if !ok {
		http.Error(w, "Pod does not exist", http.StatusNotFound)
		return
	}
	command := strings.Split(u.Query().Get("cmd"), " ")
	data, err := s.host.RunInContainer(GetPodFullName(pod), uid, container, command)
	if err != nil {
		s.error(w, err)
		return
	}
	w.Header().Add("Content-type", "text/plain")
	w.Write(data)
}

// handleExec handles requests to run a command inside a container.
func (s *Server) handleExec(w http.ResponseWriter, req *http.Request) {
	u, err := url.ParseRequestURI(req.RequestURI)
	if err != nil {
		s.error(w, err)
		return
	}
	podNamespace, podID, uid, container, err := parseContainerCoordinates(u.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pod, ok := s.host.GetPodByName(podNamespace, podID)
	if !ok {
		http.Error(w, "Pod does not exist", http.StatusNotFound)
		return
	}

	req.ParseForm()
	// start at 1 for error stream
	expectedStreams := 1
	if req.FormValue(api.ExecStdinParam) == "1" {
		expectedStreams++
	}
	if req.FormValue(api.ExecStdoutParam) == "1" {
		expectedStreams++
	}
	tty := req.FormValue(api.ExecTTYParam) == "1"
	if !tty && req.FormValue(api.ExecStderrParam) == "1" {
		expectedStreams++
	}

	if expectedStreams == 1 {
		http.Error(w, "You must specify at least 1 of stdin, stdout, stderr", http.StatusBadRequest)
		return
	}

	streamCh := make(chan httpstream.Stream)

	upgrader := spdy.NewResponseUpgrader()
	conn := upgrader.UpgradeResponse(w, req, func(stream httpstream.Stream) error {
		streamCh <- stream
		return nil
	})
	// from this point on, we can no longer call methods on w
	if conn == nil {
		// The upgrader is responsible for notifying the client of any errors that
		// occurred during upgrading. All we can do is return here at this point
		// if we weren't successful in upgrading.
		return
	}
	defer conn.Close()

	conn.SetIdleTimeout(s.host.StreamingConnectionIdleTimeout())

	// TODO find a good default timeout value
	// TODO make it configurable?
	expired := time.NewTimer(2 * time.Second)

	var errorStream, stdinStream, stdoutStream, stderrStream httpstream.Stream
	receivedStreams := 0
WaitForStreams:
	for {
		select {
		case stream := <-streamCh:
			streamType := stream.Headers().Get(api.StreamType)
			switch streamType {
			case api.StreamTypeError:
				errorStream = stream
				defer errorStream.Reset()
				receivedStreams++
			case api.StreamTypeStdin:
				stdinStream = stream
				receivedStreams++
			case api.StreamTypeStdout:
				stdoutStream = stream
				receivedStreams++
			case api.StreamTypeStderr:
				stderrStream = stream
				receivedStreams++
			default:
				glog.Errorf("Unexpected stream type: '%s'", streamType)
			}
			if receivedStreams == expectedStreams {
				break WaitForStreams
			}
		case <-expired.C:
			// TODO find a way to return the error to the user. Maybe use a separate
			// stream to report errors?
			glog.Error("Timed out waiting for client to create streams")
			return
		}
	}

	if stdinStream != nil {
		// close our half of the input stream, since we won't be writing to it
		stdinStream.Close()
	}

	err = s.host.ExecInContainer(GetPodFullName(pod), uid, container, u.Query()[api.ExecCommandParamm], stdinStream, stdoutStream, stderrStream, tty)
	if err != nil {
		msg := fmt.Sprintf("Error executing command in container: %v", err)
		glog.Error(msg)
		errorStream.Write([]byte(msg))
	}
}

func parsePodCoordinates(path string) (namespace, pod string, uid types.UID, err error) {
	parts := strings.Split(path, "/")

	if len(parts) == 4 {
		namespace = parts[2]
		pod = parts[3]
		return
	}

	if len(parts) == 5 {
		namespace = parts[2]
		pod = parts[3]
		uid = types.UID(parts[4])
		return
	}

	err = fmt.Errorf("Unexpected path %s. Expected /.../.../<namespace>/<pod> or /.../.../<namespace>/<pod>/<uid>", path)
	return
}

func (s *Server) handlePortForward(w http.ResponseWriter, req *http.Request) {
	u, err := url.ParseRequestURI(req.RequestURI)
	if err != nil {
		s.error(w, err)
		return
	}
	podNamespace, podID, uid, err := parsePodCoordinates(u.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pod, ok := s.host.GetPodByName(podNamespace, podID)
	if !ok {
		http.Error(w, "Pod does not exist", http.StatusNotFound)
		return
	}

	streamChan := make(chan httpstream.Stream, 1)
	upgrader := spdy.NewResponseUpgrader()
	conn := upgrader.UpgradeResponse(w, req, func(stream httpstream.Stream) error {
		portString := stream.Headers().Get(api.PortHeader)
		port, err := strconv.ParseUint(portString, 10, 16)
		if err != nil {
			return fmt.Errorf("Unable to parse '%s' as a port: %v", portString, err)
		}
		if port < 1 {
			return fmt.Errorf("Port '%d' must be greater than 0", port)
		}
		streamChan <- stream
		return nil
	})
	if conn == nil {
		return
	}
	defer conn.Close()
	conn.SetIdleTimeout(s.host.StreamingConnectionIdleTimeout())

	var dataStreamLock sync.Mutex
	dataStreamChans := make(map[string]chan httpstream.Stream)

Loop:
	for {
		select {
		case <-conn.CloseChan():
			break Loop
		case stream := <-streamChan:
			streamType := stream.Headers().Get(api.StreamType)
			port := stream.Headers().Get(api.PortHeader)
			dataStreamLock.Lock()
			switch streamType {
			case "error":
				ch := make(chan httpstream.Stream)
				dataStreamChans[port] = ch
				go waitForPortForwardDataStreamAndRun(GetPodFullName(pod), uid, stream, ch, s.host)
			case "data":
				ch, ok := dataStreamChans[port]
				if ok {
					ch <- stream
					delete(dataStreamChans, port)
				} else {
					glog.Errorf("Unable to locate data stream channel for port %s", port)
				}
			default:
				glog.Errorf("streamType header must be 'error' or 'data', got: '%s'", streamType)
				stream.Reset()
			}
			dataStreamLock.Unlock()
		}
	}
}

func waitForPortForwardDataStreamAndRun(pod string, uid types.UID, errorStream httpstream.Stream, dataStreamChan chan httpstream.Stream, host HostInterface) {
	defer errorStream.Reset()

	var dataStream httpstream.Stream

	select {
	case dataStream = <-dataStreamChan:
	case <-time.After(1 * time.Second):
		errorStream.Write([]byte("Timed out waiting for data stream"))
		//TODO delete from dataStreamChans[port]
		return
	}

	portString := dataStream.Headers().Get(api.PortHeader)
	port, _ := strconv.ParseUint(portString, 10, 16)
	err := host.PortForward(pod, uid, uint16(port), dataStream)
	if err != nil {
		msg := fmt.Errorf("Error forwarding port %d to pod %s, uid %v: %v", port, pod, uid, err)
		glog.Error(msg)
		errorStream.Write([]byte(msg.Error()))
	}
}

// ServeHTTP responds to HTTP requests on the Kubelet.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	defer httplog.NewLogged(req, &w).StacktraceWhen(
		httplog.StatusIsNot(
			http.StatusOK,
			http.StatusMovedPermanently,
			http.StatusTemporaryRedirect,
			http.StatusNotFound,
			http.StatusSwitchingProtocols,
		),
	).Log()
	s.mux.ServeHTTP(w, req)
}

// serveStats implements stats logic.
func (s *Server) serveStats(w http.ResponseWriter, req *http.Request) {
	// /stats/<podfullname>/<containerName> or /stats/<namespace>/<podfullname>/<uid>/<containerName>
	components := strings.Split(strings.TrimPrefix(path.Clean(req.URL.Path), "/"), "/")
	var stats *info.ContainerInfo
	var err error
	var query info.ContainerInfoRequest
	err = json.NewDecoder(req.Body).Decode(&query)
	if err != nil && err != io.EOF {
		s.error(w, err)
		return
	}
	switch len(components) {
	case 1:
		// Machine stats
		stats, err = s.host.GetRootInfo(&query)
	case 2:
		// pod stats
		// TODO(monnand) Implement this
		err = errors.New("pod level status currently unimplemented")
	case 3:
		// Backward compatibility without uid information, does not support namespace
		pod, ok := s.host.GetPodByName(api.NamespaceDefault, components[1])
		if !ok {
			http.Error(w, "Pod does not exist", http.StatusNotFound)
			return
		}
		stats, err = s.host.GetContainerInfo(GetPodFullName(pod), "", components[2], &query)
	case 5:
		pod, ok := s.host.GetPodByName(components[1], components[2])
		if !ok {
			http.Error(w, "Pod does not exist", http.StatusNotFound)
			return
		}
		stats, err = s.host.GetContainerInfo(GetPodFullName(pod), types.UID(components[3]), components[4], &query)
	default:
		http.Error(w, "unknown resource.", http.StatusNotFound)
		return
	}
	switch err {
	case nil:
		break
	case ErrNoKubeletContainers, ErrContainerNotFound:
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	default:
		s.error(w, err)
		return
	}
	if stats == nil {
		fmt.Fprint(w, "{}")
		return
	}
	data, err := json.Marshal(stats)
	if err != nil {
		s.error(w, err)
		return
	}
	w.Header().Add("Content-type", "application/json")
	w.Write(data)
	return
}

func exportPodStatus(status api.PodStatus, versioned bool) ([]byte, error) {
	if versioned {
		// TODO: support arbitrary versions here
		codec, err := findCodec("v1beta1")
		if err != nil {
			return nil, err
		}
		return codec.Encode(&api.PodStatusResult{Status: status})
	}
	return json.Marshal(status)
}

func findCodec(version string) (runtime.Codec, error) {
	versions, err := latest.InterfacesFor(version)
	if err != nil {
		return nil, err
	}
	return versions.Codec, nil
}
