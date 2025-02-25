package http

import (
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docker-slim/docker-slim/internal/app/master/config"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/container"

	log "github.com/Sirupsen/logrus"
	dockerapi "github.com/cloudimmunity/go-dockerclientx"
)

const (
	probeRetryCount = 5
)

// CustomProbe is a custom HTTP probe
type CustomProbe struct {
	PrintState         bool
	PrintPrefix        string
	Ports              []string
	Cmds               []config.HTTPProbeCmd
	RetryCount         int
	RetryWait          int
	TargetPorts        []uint16
	ProbeFull          bool
	ContainerInspector *container.Inspector
	doneChan           chan struct{}
}

// NewCustomProbe creates a new custom HTTP probe
func NewCustomProbe(inspector *container.Inspector,
	cmds []config.HTTPProbeCmd,
	retryCount int,
	retryWait int,
	targetPorts []uint16,
	probeFull bool,
	printState bool,
	printPrefix string) (*CustomProbe, error) {
	//note: the default probe should already be there if the user asked for it

	probe := &CustomProbe{
		PrintState:         printState,
		PrintPrefix:        printPrefix,
		Cmds:               cmds,
		RetryCount:         retryCount,
		RetryWait:          retryWait,
		TargetPorts:        targetPorts,
		ProbeFull:          probeFull,
		ContainerInspector: inspector,
		doneChan:           make(chan struct{}),
	}

	availablePorts := map[string]struct{}{}
	for nsPortKey, nsPortData := range inspector.ContainerInfo.NetworkSettings.Ports {
		if (nsPortKey == inspector.CmdPort) || (nsPortKey == inspector.EvtPort) {
			continue
		}

		availablePorts[nsPortData[0].HostPort] = struct{}{}
	}

	log.Debugf("HTTP probe - available ports => %+v", availablePorts)

	if len(probe.TargetPorts) > 0 {
		for _, pnum := range probe.TargetPorts {
			pspec := dockerapi.Port(fmt.Sprintf("%v/tcp", pnum))
			if _, ok := inspector.ContainerInfo.NetworkSettings.Ports[pspec]; ok {
				probe.Ports = append(probe.Ports, inspector.ContainerInfo.NetworkSettings.Ports[pspec][0].HostPort)
			} else {
				log.Debugf("HTTP probe - ignoring port => %v", pspec)
			}
		}
		log.Debugf("HTTP probe - filtered ports => %+v", probe.Ports)
	} else {
		//order the port list based on the order of the 'EXPOSE' instructions
		if len(inspector.ImageInspector.DockerfileInfo.ExposedPorts) > 0 {
			for epi := len(inspector.ImageInspector.DockerfileInfo.ExposedPorts) - 1; epi >= 0; epi-- {
				portInfo := inspector.ImageInspector.DockerfileInfo.ExposedPorts[epi]
				if strings.Index(portInfo, "/") == -1 {
					portInfo = fmt.Sprintf("%v/tcp", portInfo)
				}

				pspec := dockerapi.Port(portInfo)

				if _, ok := inspector.ContainerInfo.NetworkSettings.Ports[pspec]; ok {
					hostPort := inspector.ContainerInfo.NetworkSettings.Ports[pspec][0].HostPort
					probe.Ports = append(probe.Ports, hostPort)

					if _, ok := availablePorts[hostPort]; ok {
						log.Debugf("HTTP probe - delete exposed port from the available ports => %v (%v)", hostPort, portInfo)
						delete(availablePorts, hostPort)
					}
				} else {
					log.Debugf("HTTP probe - Unknown exposed port - %v", portInfo)
				}
			}
		}

		for k := range availablePorts {
			probe.Ports = append(probe.Ports, k)
		}

		log.Debugf("HTTP probe - probe.Ports => %+v", probe.Ports)
	}

	return probe, nil
}

// Start starts the HTTP probe instance execution
func (p *CustomProbe) Start() {
	if p.PrintState {
		fmt.Printf("%s state=http.probe.starting message='WAIT FOR HTTP PROBE TO FINISH'\n", p.PrintPrefix)
	}

	go func() {
		//TODO: need to do a better job figuring out if the target app is ready to accept connections
		time.Sleep(9 * time.Second)

		if p.PrintState {
			fmt.Printf("%s state=http.probe.running\n", p.PrintPrefix)
		}

		httpClient := &http.Client{
			Timeout: time.Second * 30,
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 30 * time.Second,
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}

		log.Info("HTTP probe started...")

		var callCount uint64
		var errCount uint64
		var okCount uint64

		for _, port := range p.Ports {
			//If it's ok stop after the first successful probe pass
			if okCount > 0 && !p.ProbeFull {
				break
			}

			for _, cmd := range p.Cmds {
				reqBody := strings.NewReader(cmd.Body)

				var protocols []string
				if cmd.Protocol == "" {
					protocols = []string{"http", "https"}
				} else {
					protocols = []string{cmd.Protocol}
				}

				for _, proto := range protocols {
					addr := fmt.Sprintf("%s://%v:%v%v", proto, p.ContainerInspector.DockerHostIP, port, cmd.Resource)

					maxRetryCount := probeRetryCount
					if p.RetryCount > 0 {
						maxRetryCount = p.RetryCount
					}

					notReadyErrorWait := time.Duration(16)
					webErrorWait := time.Duration(8)
					otherErrorWait := time.Duration(4)
					if p.RetryWait > 0 {
						webErrorWait = time.Duration(p.RetryWait)
						notReadyErrorWait = time.Duration(p.RetryWait * 2)
						otherErrorWait = time.Duration(p.RetryWait / 2)
					}

					for i := 0; i < maxRetryCount; i++ {
						req, err := http.NewRequest(cmd.Method, addr, reqBody)
						for _, hline := range cmd.Headers {
							hparts := strings.SplitN(hline, ":", 2)
							if len(hparts) != 2 {
								log.Debugf("ignoring malformed header (%v)", hline)
								continue
							}

							hname := strings.TrimSpace(hparts[0])
							hvalue := strings.TrimSpace(hparts[1])
							req.Header.Add(hname, hvalue)
						}

						if (cmd.Username != "") || (cmd.Password != "") {
							req.SetBasicAuth(cmd.Username, cmd.Password)
						}

						res, err := httpClient.Do(req)
						callCount++
						reqBody.Seek(0, 0)

						if res != nil {
							if res.Body != nil {
								io.Copy(ioutil.Discard, res.Body)
							}

							defer res.Body.Close()
						}

						statusCode := "error"
						callErrorStr := ""
						if err == nil {
							statusCode = fmt.Sprintf("%v", res.StatusCode)
						} else {
							callErrorStr = fmt.Sprintf("error='%v'", err.Error())
						}

						if p.PrintState {
							fmt.Printf("%s info=http.probe.call status=%v method=%v target=%v attempt=%v %v time=%v\n",
								p.PrintPrefix,
								statusCode,
								cmd.Method,
								addr,
								i+1,
								callErrorStr,
								time.Now().UTC().Format(time.RFC3339))
						}

						if err == nil {
							okCount++
							break
						} else {
							errCount++

							if urlErr, ok := err.(*url.Error); ok {
								if urlErr.Err == io.EOF {
									log.Debugf("HTTP probe - target not ready yet (retry again later)...")
									time.Sleep(notReadyErrorWait * time.Second)
								} else {
									log.Debugf("HTTP probe - web error... retry again later...")
									time.Sleep(webErrorWait * time.Second)

								}
							} else {
								log.Debugf("HTTP probe - other error... retry again later...")
								time.Sleep(otherErrorWait * time.Second)
							}
						}

					}
				}
			}
		}

		log.Info("HTTP probe done.")

		if p.PrintState {
			fmt.Printf("%s info=http.probe.summary total=%v failures=%v successful=%v\n",
				p.PrintPrefix, callCount, errCount, okCount)

			warning := ""
			switch {
			case callCount == 0:
				warning = "warning=no.calls"
			case okCount == 0:
				warning = "warning=no.successful.calls"
			}

			fmt.Printf("%s state=http.probe.done %s\n", p.PrintPrefix, warning)
		}

		close(p.doneChan)
	}()
}

// DoneChan returns the 'done' channel for the HTTP probe instance
func (p *CustomProbe) DoneChan() <-chan struct{} {
	return p.doneChan
}
