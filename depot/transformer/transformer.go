package transformer

import (
	"errors"
	"fmt"
	"time"

	"code.cloudfoundry.org/archiver/compressor"
	"code.cloudfoundry.org/archiver/extractor"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/cacheddownloader"
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/executor/depot/log_streamer"
	"code.cloudfoundry.org/executor/depot/steps"
	"code.cloudfoundry.org/executor/depot/uploader"
	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/workpool"
	"github.com/tedsuo/ifrit"
)

const (
	healthCheckNofiles             uint64 = 1024
	DefaultDeclarativeCheckTimeout        = int(1 * time.Second / time.Millisecond)
)

var ErrNoCheck = errors.New("no check configured")

//go:generate counterfeiter -o faketransformer/fake_transformer.go . Transformer

type Transformer interface {
	StepFor(log_streamer.LogStreamer, *models.Action, garden.Container, string, string, []executor.PortMapping, lager.Logger) steps.Step
	StepsRunner(lager.Logger, executor.Container, garden.Container, log_streamer.LogStreamer) (ifrit.Runner, error)
}

type transformer struct {
	cachedDownloader     cacheddownloader.CachedDownloader
	uploader             uploader.Uploader
	extractor            extractor.Extractor
	compressor           compressor.Compressor
	downloadLimiter      chan struct{}
	uploadLimiter        chan struct{}
	tempDir              string
	exportNetworkEnvVars bool
	clock                clock.Clock

	useDeclarativeHealthCheck bool

	postSetupHook []string
	postSetupUser string

	healthyMonitoringInterval   time.Duration
	unhealthyMonitoringInterval time.Duration
	healthCheckWorkPool         *workpool.WorkPool
}

func NewTransformer(
	cachedDownloader cacheddownloader.CachedDownloader,
	uploader uploader.Uploader,
	extractor extractor.Extractor,
	compressor compressor.Compressor,
	downloadLimiter chan struct{},
	uploadLimiter chan struct{},
	tempDir string,
	exportNetworkEnvVars bool,
	healthyMonitoringInterval time.Duration,
	unhealthyMonitoringInterval time.Duration,
	healthCheckWorkPool *workpool.WorkPool,
	clock clock.Clock,
	postSetupHook []string,
	postSetupUser string,
	useDeclarativeHealthCheck bool,
) *transformer {
	return &transformer{
		cachedDownloader:            cachedDownloader,
		uploader:                    uploader,
		extractor:                   extractor,
		compressor:                  compressor,
		downloadLimiter:             downloadLimiter,
		uploadLimiter:               uploadLimiter,
		tempDir:                     tempDir,
		exportNetworkEnvVars:        exportNetworkEnvVars,
		healthyMonitoringInterval:   healthyMonitoringInterval,
		unhealthyMonitoringInterval: unhealthyMonitoringInterval,
		healthCheckWorkPool:         healthCheckWorkPool,
		clock:                       clock,
		postSetupHook:               postSetupHook,
		postSetupUser:               postSetupUser,
		useDeclarativeHealthCheck:   useDeclarativeHealthCheck,
	}
}

func (t *transformer) StepFor(
	logStreamer log_streamer.LogStreamer,
	action *models.Action,
	container garden.Container,
	externalIP string,
	internalIP string,
	ports []executor.PortMapping,
	logger lager.Logger,
) steps.Step {
	a := action.GetValue()
	switch actionModel := a.(type) {
	case *models.RunAction:
		return steps.NewRun(
			container,
			*actionModel,
			logStreamer.WithSource(actionModel.LogSource),
			logger,
			externalIP,
			internalIP,
			ports,
			t.exportNetworkEnvVars,
			t.clock,
		)

	case *models.DownloadAction:
		return steps.NewDownload(
			container,
			*actionModel,
			t.cachedDownloader,
			t.downloadLimiter,
			logStreamer.WithSource(actionModel.LogSource),
			logger,
		)

	case *models.UploadAction:
		return steps.NewUpload(
			container,
			*actionModel,
			t.uploader,
			t.compressor,
			t.tempDir,
			logStreamer.WithSource(actionModel.LogSource),
			t.uploadLimiter,
			logger,
		)

	case *models.EmitProgressAction:
		return steps.NewEmitProgress(
			t.StepFor(
				logStreamer,
				actionModel.Action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			),
			actionModel.StartMessage,
			actionModel.SuccessMessage,
			actionModel.FailureMessagePrefix,
			logStreamer.WithSource(actionModel.LogSource),
			logger,
		)

	case *models.TimeoutAction:
		return steps.NewTimeout(
			t.StepFor(
				logStreamer.WithSource(actionModel.LogSource),
				actionModel.Action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			),
			time.Duration(actionModel.TimeoutMs)*time.Millisecond,
			logger,
		)

	case *models.TryAction:
		return steps.NewTry(
			t.StepFor(
				logStreamer.WithSource(actionModel.LogSource),
				actionModel.Action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			),
			logger,
		)

	case *models.ParallelAction:
		subSteps := make([]steps.Step, len(actionModel.Actions))
		for i, action := range actionModel.Actions {
			subSteps[i] = t.StepFor(
				logStreamer.WithSource(actionModel.LogSource),
				action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			)
		}
		return steps.NewParallel(subSteps)

	case *models.CodependentAction:
		subSteps := make([]steps.Step, len(actionModel.Actions))
		for i, action := range actionModel.Actions {
			subSteps[i] = t.StepFor(
				logStreamer.WithSource(actionModel.LogSource),
				action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			)
		}
		errorOnExit := true
		return steps.NewCodependent(subSteps, errorOnExit)

	case *models.SerialAction:
		subSteps := make([]steps.Step, len(actionModel.Actions))
		for i, action := range actionModel.Actions {
			subSteps[i] = t.StepFor(
				logStreamer,
				action,
				container,
				externalIP,
				internalIP,
				ports,
				logger,
			)
		}
		return steps.NewSerial(subSteps)
	}

	panic(fmt.Sprintf("unknown action: %T", action))
}

func overrideSuppressLogOutput(monitorAction *models.Action) {
	if monitorAction.RunAction != nil {
		monitorAction.RunAction.SuppressLogOutput = false
	} else if monitorAction.TryAction != nil {
		overrideSuppressLogOutput(monitorAction.TryAction.Action)
	} else if monitorAction.ParallelAction != nil {
		for _, action := range monitorAction.ParallelAction.Actions {
			overrideSuppressLogOutput(action)
		}
	} else if monitorAction.SerialAction != nil {
		for _, action := range monitorAction.SerialAction.Actions {
			overrideSuppressLogOutput(action)
		}
	} else if monitorAction.CodependentAction != nil {
		for _, action := range monitorAction.CodependentAction.Actions {
			overrideSuppressLogOutput(action)
		}
	} else if monitorAction.EmitProgressAction != nil {
		overrideSuppressLogOutput(monitorAction.EmitProgressAction.Action)
	} else if monitorAction.TimeoutAction != nil {
		overrideSuppressLogOutput(monitorAction.TimeoutAction.Action)
	}
}
func (t *transformer) StepsRunner(
	logger lager.Logger,
	container executor.Container,
	gardenContainer garden.Container,
	logStreamer log_streamer.LogStreamer,
) (ifrit.Runner, error) {
	var setup, action, postSetup, monitor steps.Step
	if container.Setup != nil {
		setup = t.StepFor(
			logStreamer,
			container.Setup,
			gardenContainer,
			container.ExternalIP,
			container.InternalIP,
			container.Ports,
			logger.Session("setup"),
		)
	}

	if len(t.postSetupHook) > 0 {
		actionModel := models.RunAction{
			Path: t.postSetupHook[0],
			Args: t.postSetupHook[1:],
			User: t.postSetupUser,
		}
		postSetup = steps.NewRun(
			gardenContainer,
			actionModel,
			log_streamer.NewNoopStreamer(),
			logger,
			container.ExternalIP,
			container.InternalIP,
			container.Ports,
			t.exportNetworkEnvVars,
			t.clock,
		)
	}

	if container.Action == nil {
		err := errors.New("container cannot have empty action")
		logger.Error("steps-runner-empty-action", err)
		return nil, err
	}

	action = t.StepFor(
		logStreamer,
		container.Action,
		gardenContainer,
		container.ExternalIP,
		container.InternalIP,
		container.Ports,
		logger.Session("action"),
	)

	hasStartedRunning := make(chan struct{}, 1)

	if container.CheckDefinition != nil && t.useDeclarativeHealthCheck {
		monitor = t.transformCheckDefinition(logger, &container, gardenContainer, hasStartedRunning, logStreamer)
	} else if container.Monitor != nil {
		overrideSuppressLogOutput(container.Monitor)
		monitor = steps.NewMonitor(
			func(monitorStreamer log_streamer.LogStreamer) steps.Step {
				return t.StepFor(
					monitorStreamer,
					container.Monitor,
					gardenContainer,
					container.ExternalIP,
					container.InternalIP,
					container.Ports,
					logger.Session("monitor-run"),
				)
			},
			hasStartedRunning,
			logger.Session("monitor"),
			t.clock,
			logStreamer,
			time.Duration(container.StartTimeoutMs)*time.Millisecond,
			t.healthyMonitoringInterval,
			t.unhealthyMonitoringInterval,
			t.healthCheckWorkPool,
		)
	}

	var longLivedAction steps.Step
	if monitor != nil {
		longLivedAction = steps.NewCodependent([]steps.Step{action, monitor}, false)
	} else {
		longLivedAction = action

		// this container isn't monitored, so we mark it running right away
		hasStartedRunning <- struct{}{}
	}

	var step steps.Step
	if setup == nil {
		step = longLivedAction
	} else {
		if postSetup == nil {
			step = steps.NewSerial([]steps.Step{setup, longLivedAction})
		} else {
			step = steps.NewSerial([]steps.Step{setup, postSetup, longLivedAction})
		}
	}

	return newStepRunner(step, hasStartedRunning), nil
}

func (t *transformer) transformCheckDefinition(
	logger lager.Logger,
	container *executor.Container,
	gardenContainer garden.Container,
	hasStartedRunning chan<- struct{},
	logstreamer log_streamer.LogStreamer,
) steps.Step {
	var readinessChecks []models.ActionInterface
	var livenessChecks []models.ActionInterface

	nofiles := healthCheckNofiles

	logger.Info("transform-check-definitions-starting")
	defer func() {
		logger.Info("transform-check-definitions-finished")
	}()

	createCheck := func(path string, port, timeout int, http, readiness bool, interval time.Duration) models.ActionInterface {
		args := []string{
			fmt.Sprintf("-port=%d", port),
			fmt.Sprintf("-timeout=%dms", timeout),
		}

		if http {
			args = append(args, fmt.Sprintf("-uri=%s", path))
		}

		if readiness {
			args = append(args, fmt.Sprintf("-readiness-interval=%s", interval))
		} else {
			args = append(args, fmt.Sprintf("-liveness-interval=%s", interval))
		}

		runAction := &models.RunAction{
			User:           "root",
			LogSource:      "HEALTH",
			ResourceLimits: &models.ResourceLimits{Nofile: &nofiles},
			Path:           "/etc/cf-assets/healthcheck/healthcheck",
			Args:           args,
		}

		return runAction
	}

	for _, check := range container.CheckDefinition.Checks {
		if err := check.Validate(); err != nil {
			logger.Error("invalid-check", err, lager.Data{"check": check})
		} else if check.HttpCheck != nil {
			timeout := int(check.HttpCheck.RequestTimeoutMs)
			if timeout == 0 {
				timeout = DefaultDeclarativeCheckTimeout
			}
			path := check.HttpCheck.Path
			if path == "" {
				path = "/"
			}
			readinessChecks = append(readinessChecks, createCheck(path, int(check.HttpCheck.Port), timeout, true, true, t.unhealthyMonitoringInterval))
			livenessChecks = append(livenessChecks, createCheck(path, int(check.HttpCheck.Port), timeout, true, false, t.healthyMonitoringInterval))
		} else if check.TcpCheck != nil {
			timeout := int(check.TcpCheck.ConnectTimeoutMs)
			if timeout == 0 {
				timeout = DefaultDeclarativeCheckTimeout
			}

			readinessChecks = append(readinessChecks, createCheck("", int(check.TcpCheck.Port), timeout, false, true, t.unhealthyMonitoringInterval))
			livenessChecks = append(livenessChecks, createCheck("", int(check.TcpCheck.Port), timeout, false, false, t.healthyMonitoringInterval))
		}
	}

	readinessCheckFunc := func(logstreamer log_streamer.LogStreamer) steps.Step {
		logger := logger.Session("readiness-check")
		action := models.WrapAction(models.Parallel(readinessChecks...))
		return t.StepFor(logstreamer, action, gardenContainer, container.ExternalIP, container.InternalIP, container.Ports, logger)
	}
	livenessCheckFunc := func(logstreamer log_streamer.LogStreamer) steps.Step {
		logger := logger.Session("liveness-check")
		action := models.WrapAction(models.Codependent(livenessChecks...))
		return t.StepFor(logstreamer, action, gardenContainer, container.ExternalIP, container.InternalIP, container.Ports, logger)
	}

	return steps.NewLongRunningMonitor(
		readinessCheckFunc,
		livenessCheckFunc,
		hasStartedRunning,
		logger,
		t.clock,
		logstreamer,
		time.Duration(container.StartTimeoutMs)*time.Millisecond,
		t.healthCheckWorkPool,
	)
}
