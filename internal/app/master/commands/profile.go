package commands

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"github.com/docker-slim/docker-slim/internal/app/master/config"
	"github.com/docker-slim/docker-slim/internal/app/master/docker/dockerclient"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/container"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/container/probes/http"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/image"
	"github.com/docker-slim/docker-slim/internal/app/master/version"
	"github.com/docker-slim/docker-slim/pkg/report"
	"github.com/docker-slim/docker-slim/pkg/util/errutil"
	"github.com/docker-slim/docker-slim/pkg/util/fsutil"
	v "github.com/docker-slim/docker-slim/pkg/version"

	log "github.com/Sirupsen/logrus"
	"github.com/dustin/go-humanize"
)

// OnProfile implements the 'profile' docker-slim command
func OnProfile(
	doCheckVersion bool,
	cmdReportLocation string,
	doDebug bool,
	statePath string,
	clientConfig *config.DockerClient,
	imageRef string,
	doHTTPProbe bool,
	httpProbeCmds []config.HTTPProbeCmd,
	httpProbeRetryCount int,
	httpProbeRetryWait int,
	httpProbePorts []uint16,
	doHTTPProbeFull bool,
	copyMetaArtifactsLocation string,
	doShowContainerLogs bool,
	overrides *config.ContainerOverrides,
	links []string,
	etcHostsMaps []string,
	dnsServers []string,
	dnsSearchDomains []string,
	volumeMounts map[string]config.VolumeMount,
	excludePaths map[string]bool,
	includePaths map[string]bool,
	includeBins map[string]bool,
	includeExes map[string]bool,
	doIncludeShell bool,
	continueAfter *config.ContinueAfter) {
	logger := log.WithFields(log.Fields{"app": "docker-slim", "command": "profile"})

	viChan := version.CheckAsync(doCheckVersion)

	cmdReport := report.NewProfileCommand(cmdReportLocation)
	cmdReport.State = report.CmdStateStarted
	cmdReport.OriginalImage = imageRef

	fmt.Println("docker-slim[profile]: state=started")
	fmt.Printf("docker-slim[profile]: info=params target=%v\n", imageRef)
	doRmFileArtifacts := false

	client := dockerclient.New(clientConfig)

	if doDebug {
		version.Print(client, false)
	}

	if !confirmNetwork(logger, client, overrides.Network) {
		fmt.Printf("docker-slim[profile]: info=param.error status=unknown.network value=%s\n", overrides.Network)
		fmt.Printf("docker-slim[profile]: state=exited version=%s\n", v.Current())
		os.Exit(-111)
	}

	imageInspector, err := image.NewInspector(client, imageRef)
	errutil.FailOn(err)

	if imageInspector.NoImage() {
		fmt.Println("docker-slim[profile]: target image not found -", imageRef)
		fmt.Println("docker-slim[profile]: state=exited")
		return
	}

	fmt.Println("docker-slim[profile]: state=image.inspection.start")

	logger.Info("inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	errutil.FailOn(err)

	localVolumePath, artifactLocation, statePath := fsutil.PrepareImageStateDirs(statePath, imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation

	fmt.Printf("docker-slim[profile]: info=image id=%v size.bytes=%v size.human=%v\n",
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	logger.Info("processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	errutil.FailOn(err)

	fmt.Println("docker-slim[profile]: state=image.inspection.done")
	fmt.Println("docker-slim[profile]: state=container.inspection.start")

	containerInspector, err := container.NewInspector(client,
		statePath,
		imageInspector,
		localVolumePath,
		overrides,
		links,
		etcHostsMaps,
		dnsServers,
		dnsSearchDomains,
		doShowContainerLogs,
		volumeMounts,
		excludePaths,
		includePaths,
		includeBins,
		includeExes,
		doIncludeShell,
		doDebug,
		true,
		"docker-slim[profile]:")
	errutil.FailOn(err)

	logger.Info("starting instrumented 'fat' container...")
	err = containerInspector.RunContainer()
	errutil.FailOn(err)

	fmt.Printf("docker-slim[build]: info=container name=%v id=%v target.port.list=[%v] target.port.info=[%v] message='YOU CAN USE THESE PORTS TO INTERACT WITH THE CONTAINER'\n",
		containerInspector.ContainerName,
		containerInspector.ContainerID,
		containerInspector.ContainerPortList,
		containerInspector.ContainerPortsInfo)

	logger.Info("watching container monitor...")

	if "probe" == continueAfter.Mode {
		doHTTPProbe = true
	}

	if doHTTPProbe {
		probe, err := http.NewCustomProbe(containerInspector, httpProbeCmds,
			httpProbeRetryCount, httpProbeRetryWait, httpProbePorts, doHTTPProbeFull,
			true, "docker-slim[profile]:")
		errutil.FailOn(err)
		if len(probe.Ports) == 0 {
			fmt.Println("docker-slim[profile]: state=http.probe.error error='no exposed ports' message='expose your service port with --expose or disable HTTP probing with --http-probe=false if your containerized application doesnt expose any network services")
			logger.Info("shutting down 'fat' container...")
			containerInspector.FinishMonitoring()
			_ = containerInspector.ShutdownContainer()

			fmt.Println("docker-slim[profile]: state=exited")
			return
		}

		probe.Start()
		continueAfter.ContinueChan = probe.DoneChan()
	}

	switch continueAfter.Mode {
	case "enter":
		fmt.Println("docker-slim[profile]: info=prompt message='USER INPUT REQUIRED, PRESS <ENTER> WHEN YOU ARE DONE USING THE CONTAINER'")
		creader := bufio.NewReader(os.Stdin)
		_, _, _ = creader.ReadLine()
	case "signal":
		fmt.Println("docker-slim[profile]: info=prompt message='send SIGUSR1 when you are done using the container'")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim[profile]: info=event message='got SIGUSR1'")
	case "timeout":
		fmt.Printf("docker-slim[profile]: info=prompt message='waiting for the target container (%v seconds)'\n", int(continueAfter.Timeout))
		<-time.After(time.Second * continueAfter.Timeout)
		fmt.Printf("docker-slim[profile]: info=event message='done waiting for the target container'")
	case "probe":
		fmt.Println("docker-slim[profile]: info=prompt message='waiting for the HTTP probe to finish'")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim[profile]: info=event message='HTTP probe is done'")
	default:
		errutil.Fail("unknown continue-after mode")
	}

	fmt.Println("docker-slim[profile]: state=container.inspection.finishing")

	containerInspector.FinishMonitoring()

	logger.Info("shutting down 'fat' container...")
	err = containerInspector.ShutdownContainer()
	errutil.WarnOn(err)

	fmt.Println("docker-slim[profile]: state=container.inspection.artifact.processing")

	if !containerInspector.HasCollectedData() {
		imageInspector.ShowFatImageDockerInstructions()
		fmt.Printf("docker-slim[profile]: info=results status='no data collected (no minified image generated). (version: %v)'\n",
			v.Current())
		fmt.Println("docker-slim[profile]: state=exited")
		return
	}

	logger.Info("processing instrumented 'fat' container info...")
	err = containerInspector.ProcessCollectedData()
	errutil.FailOn(err)

	fmt.Println("docker-slim[profile]: state=container.inspection.done")
	fmt.Println("docker-slim[profile]: state=completed")
	cmdReport.State = report.CmdStateCompleted

	if copyMetaArtifactsLocation != "" {
		toCopy := []string{
			report.DefaultContainerReportFileName,
			imageInspector.SeccompProfileName,
			imageInspector.AppArmorProfileName,
		}
		if !copyMetaArtifacts(logger,
			toCopy,
			imageInspector.ArtifactLocation, copyMetaArtifactsLocation) {
			fmt.Println("docker-slim[profile]: info=artifacts message='could not copy meta artifacts'")
		}
	}

	if doRmFileArtifacts {
		logger.Info("removing temporary artifacts...")
		err = fsutil.Remove(artifactLocation) //TODO: remove only the "files" subdirectory
		errutil.WarnOn(err)
	}

	fmt.Println("docker-slim[profile]: state=done")

	vinfo := <-viChan
	version.PrintCheckVersion(vinfo)

	cmdReport.State = report.CmdStateDone
	cmdReport.Save()
}
