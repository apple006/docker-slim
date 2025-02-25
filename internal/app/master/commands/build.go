package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker-slim/docker-slim/internal/app/master/builder"
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

// OnBuild implements the 'build' docker-slim command
func OnBuild(
	doCheckVersion bool,
	cmdReportLocation string,
	doDebug bool,
	statePath string,
	clientConfig *config.DockerClient,
	buildFromDockerfile string,
	imageRef string,
	customImageTag string,
	doHTTPProbe bool,
	httpProbeCmds []config.HTTPProbeCmd,
	httpProbeRetryCount int,
	httpProbeRetryWait int,
	httpProbePorts []uint16,
	doHTTPProbeFull bool,
	doRmFileArtifacts bool,
	copyMetaArtifactsLocation string,
	doShowContainerLogs bool,
	doShowBuildLogs bool,
	imageOverrideSelectors map[string]bool,
	overrides *config.ContainerOverrides,
	instructions *config.ImageNewInstructions,
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
	logger := log.WithFields(log.Fields{"app": "docker-slim", "command": "build"})

	viChan := version.CheckAsync(doCheckVersion)

	cmdReport := report.NewBuildCommand(cmdReportLocation)
	cmdReport.State = report.CmdStateStarted
	cmdReport.ImageReference = imageRef

	client := dockerclient.New(clientConfig)

	fmt.Println("docker-slim[build]: state=started")
	if buildFromDockerfile == "" {
		fmt.Printf("docker-slim[build]: info=params target=%v continue.mode=%v\n", imageRef, continueAfter.Mode)
	} else {
		fmt.Printf("docker-slim[build]: info=params context=%v/file=%v continue.mode=%v\n", imageRef, buildFromDockerfile, continueAfter.Mode)
	}

	if buildFromDockerfile != "" {
		fmt.Println("docker-slim[build]: state=building message='building basic image'")
		//create a fat image name based on the user provided custom tag if it's available
		var fatImageRepoNameTag string
		if customImageTag != "" {
			citParts := strings.Split(customImageTag, ":")
			switch len(citParts) {
			case 1:
				fatImageRepoNameTag = fmt.Sprintf("%s.fat", customImageTag)
			case 2:
				fatImageRepoNameTag = fmt.Sprintf("%s.fat:%s", citParts[0], citParts[1])
			default:
				fmt.Printf("docker-slim[build]: info=param.error status=malformed.custom.image.tag value=%s\n", customImageTag)
				fmt.Printf("docker-slim[build]: state=exited version=%s\n", v.Current())
				os.Exit(-1)
			}
		} else {
			fatImageRepoNameTag = fmt.Sprintf("docker-slim-tmp-fat-image.%v.%v",
				os.Getpid(), time.Now().UTC().Format("20060102150405"))
		}

		fmt.Printf("docker-slim[build]: info=basic.image.name value=%s\n", fatImageRepoNameTag)

		fatBuilder, err := builder.NewBasicImageBuilder(client,
			fatImageRepoNameTag,
			buildFromDockerfile,
			imageRef,
			doShowBuildLogs)
		errutil.FailOn(err)

		err = fatBuilder.Build()

		if doShowBuildLogs {
			fmt.Println("docker-slim[build]: build logs (basic image) ====================")
			fmt.Println(fatBuilder.BuildLog.String())
			fmt.Println("docker-slim[build]: end of build logs (basic image) =============")
		}

		errutil.FailOn(err)

		fmt.Println("docker-slim[build]: state=basic.image.build.completed")

		imageRef = fatImageRepoNameTag
		//todo: remove the temporary fat image (should have a flag for it in case users want the fat image too)
	}

	logger.Infof("image=%v http-probe=%v remove-file-artifacts=%v image-overrides=%+v entrypoint=%+v (%v) cmd=%+v (%v) workdir='%v' env=%+v expose=%+v",
		imageRef, doHTTPProbe, doRmFileArtifacts,
		imageOverrideSelectors,
		overrides.Entrypoint, overrides.ClearEntrypoint, overrides.Cmd, overrides.ClearCmd,
		overrides.Workdir, overrides.Env, overrides.ExposedPorts)

	if doDebug {
		version.Print(client, false)
	}

	if !confirmNetwork(logger, client, overrides.Network) {
		fmt.Printf("docker-slim[build]: info=param.error status=unknown.network value=%s\n", overrides.Network)
		fmt.Printf("docker-slim[build]: state=exited version=%s\n", v.Current())
		os.Exit(-111)
	}

	imageInspector, err := image.NewInspector(client, imageRef)
	errutil.FailOn(err)

	if imageInspector.NoImage() {
		fmt.Println("docker-slim[build]: target image not found -", imageRef)
		fmt.Println("docker-slim[build]: state=exited")
		return
	}

	fmt.Println("docker-slim[build]: state=image.inspection.start")

	logger.Info("inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	errutil.FailOn(err)

	localVolumePath, artifactLocation, statePath := fsutil.PrepareImageStateDirs(statePath, imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation

	fmt.Printf("docker-slim[build]: info=image id=%v size.bytes=%v size.human=%v\n",
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	logger.Info("processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	errutil.FailOn(err)

	if imageInspector.DockerfileInfo != nil {
		if imageInspector.DockerfileInfo.ExeUser != "" {
			fmt.Printf("docker-slim[build]: info=image.users exe='%v' all='%v'\n",
				imageInspector.DockerfileInfo.ExeUser,
				strings.Join(imageInspector.DockerfileInfo.AllUsers, ","))
		}

		if len(imageInspector.DockerfileInfo.ImageStack) > 0 {
			cmdReport.ImageStack = imageInspector.DockerfileInfo.ImageStack

			for idx, layerInfo := range imageInspector.DockerfileInfo.ImageStack {
				fmt.Printf("docker-slim[build]: info=image.stack index=%v name='%v' id='%v'\n",
					idx, layerInfo.FullName, layerInfo.ID)
			}
		}

		if len(imageInspector.DockerfileInfo.ExposedPorts) > 0 {
			fmt.Printf("docker-slim[build]: info=image.exposed_ports list='%v'\n",
				strings.Join(imageInspector.DockerfileInfo.ExposedPorts, ","))
		}
	}

	fmt.Println("docker-slim[build]: state=image.inspection.done")
	fmt.Println("docker-slim[build]: state=container.inspection.start")

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
		"docker-slim[build]:")
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
			true, "docker-slim[build]:")
		errutil.FailOn(err)
		if len(probe.Ports) == 0 {
			fmt.Println("docker-slim[build]: state=http.probe.error error='no exposed ports' message='expose your service port with --expose or disable HTTP probing with --http-probe=false if your containerized application doesnt expose any network services")
			logger.Info("shutting down 'fat' container...")
			containerInspector.FinishMonitoring()
			_ = containerInspector.ShutdownContainer()

			fmt.Println("docker-slim[build]: state=exited")
			return
		}

		probe.Start()
		continueAfter.ContinueChan = probe.DoneChan()
	}

	switch continueAfter.Mode {
	case "enter":
		fmt.Println("docker-slim[build]: info=prompt message='USER INPUT REQUIRED, PRESS <ENTER> WHEN YOU ARE DONE USING THE CONTAINER'")
		creader := bufio.NewReader(os.Stdin)
		_, _, _ = creader.ReadLine()
	case "signal":
		fmt.Println("docker-slim[build]: info=prompt message='send SIGUSR1 when you are done using the container'")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim[build]: info=event message='got SIGUSR1'")
	case "timeout":
		fmt.Printf("docker-slim[build]: info=prompt message='waiting for the target container (%v seconds)'\n", int(continueAfter.Timeout))
		<-time.After(time.Second * continueAfter.Timeout)
		fmt.Printf("docker-slim[build]: info=event message='done waiting for the target container'")
	case "probe":
		fmt.Println("docker-slim[build]: info=prompt message='waiting for the HTTP probe to finish'")
		<-continueAfter.ContinueChan
		fmt.Println("docker-slim[build]: info=event message='HTTP probe is done'")
	default:
		errutil.Fail("unknown continue-after mode")
	}

	fmt.Println("docker-slim[build]: state=container.inspection.finishing")

	containerInspector.FinishMonitoring()

	logger.Info("shutting down 'fat' container...")
	err = containerInspector.ShutdownContainer()
	errutil.WarnOn(err)

	fmt.Println("docker-slim[build]: state=container.inspection.artifact.processing")

	if !containerInspector.HasCollectedData() {
		imageInspector.ShowFatImageDockerInstructions()
		fmt.Printf("docker-slim[build]: info=results status='no data collected (no minified image generated). (version: %v)'\n",
			v.Current())
		fmt.Println("docker-slim[build]: state=exited")
		return
	}

	logger.Info("processing instrumented 'fat' container info...")
	err = containerInspector.ProcessCollectedData()
	errutil.FailOn(err)

	if customImageTag == "" {
		customImageTag = imageInspector.SlimImageRepo
	}

	fmt.Println("docker-slim[build]: state=container.inspection.done")
	fmt.Println("docker-slim[build]: state=building message='building minified image'")

	builder, err := builder.NewImageBuilder(client,
		customImageTag,
		imageInspector.ImageInfo,
		artifactLocation,
		doShowBuildLogs,
		imageOverrideSelectors,
		overrides,
		instructions)
	errutil.FailOn(err)

	if !builder.HasData {
		logger.Info("WARNING - no data artifacts")
	}

	err = builder.Build()

	if doShowBuildLogs {
		fmt.Println("docker-slim[build]: build logs ====================")
		fmt.Println(builder.BuildLog.String())
		fmt.Println("docker-slim[build]: end of build logs =============")
	}

	errutil.FailOn(err)

	fmt.Println("docker-slim[build]: state=completed")
	cmdReport.State = report.CmdStateCompleted

	/////////////////////////////
	newImageInspector, err := image.NewInspector(client, builder.RepoName)
	errutil.FailOn(err)

	if newImageInspector.NoImage() {
		fmt.Printf("docker-slim[build]: info=results message='minified image not found - %s'\n", builder.RepoName)
		fmt.Println("docker-slim[build]: state=exited")
		return
	}

	err = newImageInspector.Inspect()
	errutil.WarnOn(err)

	if err == nil {
		cmdReport.MinifiedBy = float64(imageInspector.ImageInfo.VirtualSize) / float64(newImageInspector.ImageInfo.VirtualSize)

		cmdReport.SourceImage = report.ImageMetadata{
			AllNames:      imageInspector.ImageRecordInfo.RepoTags,
			ID:            imageInspector.ImageRecordInfo.ID,
			Size:          imageInspector.ImageInfo.VirtualSize,
			SizeHuman:     humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)),
			CreateTime:    imageInspector.ImageInfo.Created.UTC().Format(time.RFC3339),
			Author:        imageInspector.ImageInfo.Author,
			DockerVersion: imageInspector.ImageInfo.DockerVersion,
			Architecture:  imageInspector.ImageInfo.Architecture,
			User:          imageInspector.ImageInfo.Config.User,
		}

		if len(imageInspector.ImageRecordInfo.RepoTags) > 0 {
			cmdReport.SourceImage.Name = imageInspector.ImageRecordInfo.RepoTags[0]
		}

		if len(imageInspector.ImageInfo.Config.ExposedPorts) > 0 {
			for k := range imageInspector.ImageInfo.Config.ExposedPorts {
				cmdReport.SourceImage.ExposedPorts = append(cmdReport.SourceImage.ExposedPorts, string(k))
			}
		}

		cmdReport.MinifiedImageSize = newImageInspector.ImageInfo.VirtualSize
		cmdReport.MinifiedImageSizeHuman = humanize.Bytes(uint64(newImageInspector.ImageInfo.VirtualSize))

		fmt.Printf("docker-slim[build]: info=results status='MINIFIED BY %.2fX [%v (%v) => %v (%v)]'\n",
			cmdReport.MinifiedBy,
			cmdReport.SourceImage.Size,
			cmdReport.SourceImage.SizeHuman,
			cmdReport.MinifiedImageSize,
			cmdReport.MinifiedImageSizeHuman)
	} else {
		cmdReport.State = report.CmdStateError
		cmdReport.Error = err.Error()
	}

	cmdReport.MinifiedImage = builder.RepoName
	cmdReport.MinifiedImageHasData = builder.HasData
	cmdReport.ArtifactLocation = imageInspector.ArtifactLocation
	cmdReport.ContainerReportName = report.DefaultContainerReportFileName
	cmdReport.SeccompProfileName = imageInspector.SeccompProfileName
	cmdReport.AppArmorProfileName = imageInspector.AppArmorProfileName

	fmt.Printf("docker-slim[build]: info=results  image.name=%v image.size='%v' data=%v\n",
		cmdReport.MinifiedImage,
		cmdReport.MinifiedImageSizeHuman,
		cmdReport.MinifiedImageHasData)

	fmt.Printf("docker-slim[build]: info=results  artifacts.location='%v'\n", cmdReport.ArtifactLocation)
	fmt.Printf("docker-slim[build]: info=results  artifacts.report=%v\n", cmdReport.ContainerReportName)
	fmt.Printf("docker-slim[build]: info=results  artifacts.dockerfile.original=Dockerfile.fat\n")
	fmt.Printf("docker-slim[build]: info=results  artifacts.dockerfile.new=Dockerfile\n")
	fmt.Printf("docker-slim[build]: info=results  artifacts.seccomp=%v\n", cmdReport.SeccompProfileName)
	fmt.Printf("docker-slim[build]: info=results  artifacts.apparmor=%v\n", cmdReport.AppArmorProfileName)

	if cmdReport.ArtifactLocation != "" {
		creportPath := filepath.Join(cmdReport.ArtifactLocation, cmdReport.ContainerReportName)
		if creportData, err := ioutil.ReadFile(creportPath); err == nil {
			var creport report.ContainerReport
			if err := json.Unmarshal(creportData, &creport); err == nil {
				cmdReport.System = report.SystemMetadata{
					Type:    creport.System.Type,
					Release: creport.System.Release,
					OS:      creport.System.OS,
				}
			} else {
				logger.Infof("could not read container report - json parsing error - %v", err)
			}
		} else {
			logger.Infof("could not read container report - %v", err)
		}

	}

	/////////////////////////////
	if copyMetaArtifactsLocation != "" {
		toCopy := []string{
			report.DefaultContainerReportFileName,
			imageInspector.SeccompProfileName,
			imageInspector.AppArmorProfileName,
		}
		if !copyMetaArtifacts(logger,
			toCopy,
			imageInspector.ArtifactLocation, copyMetaArtifactsLocation) {
			fmt.Println("docker-slim[build]: info=artifacts message='could not copy meta artifacts'")
		}
	}

	if doRmFileArtifacts {
		logger.Info("removing temporary artifacts...")
		err = fsutil.Remove(artifactLocation) //TODO: remove only the "files" subdirectory
		errutil.WarnOn(err)
	}

	fmt.Println("docker-slim[build]: state=done")

	vinfo := <-viChan
	version.PrintCheckVersion(vinfo)

	cmdReport.State = report.CmdStateDone
	cmdReport.Save()
}
