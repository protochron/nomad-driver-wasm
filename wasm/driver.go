package wasm

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	log "github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	pluginName          = "wasm"
	fingerprintDuration = time.Duration(30 * time.Second)
	taskHandleVersion   = 1
)

var (
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		Name:              pluginName,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     "v0.0.1",
	}

	supportedBins = []string{
		"wasmtime",
	}

	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),

		"wasm_binary_path": hclspec.NewAttr("wasm_binary_path", "string", true),
	})

	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"path": hclspec.NewAttr("path", "string", true),
	})

	capabilities = &drivers.Capabilities{}
)

type Driver struct {
	// Embed the types that disable various options this plugin doesn't support.
	drivers.DriverExecTaskNotSupported
	drivers.DriverSignalTaskNotSupported

	ctx            context.Context
	signalShutdown context.CancelFunc
	logger         log.Logger
	eventer        *eventer.Eventer
	config         *Config
	tasks          *taskStore
	nomadConfig    *base.ClientDriverConfig
}

type Config struct {
	Enabled    bool   `codec:"enabled"`
	BinaryPath string `codec:"binary_path"`
}

type TaskConfig struct {
	Path string   `codec:"path"`
	Args []string `codec:"args"`
}

type TaskState struct {
	ReattachConfig *structs.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	StartedAt      time.Time
	Pid            int
}

func NewWasmTask(log log.Logger) *Driver {
	ctx, cancel := context.WithCancel(context.Background())
	logger := log.Named(pluginName)

	return &Driver{
		config:         &Config{},
		ctx:            ctx,
		eventer:        eventer.NewEventer(ctx, log),
		logger:         logger,
		signalShutdown: cancel,
		tasks:          newTaskStore(),
	}
}

// PluginInfo describes the type and version of a plugin.
func (w *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

// ConfigSchema returns the schema for parsing the plugins configuration.
func (w *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

// SetConfig is used to set the configuration by passing a MessagePack
// encoding of it.
func (d *Driver) SetConfig(c *base.Config) error {
	var config Config
	if len(c.PluginConfig) != 0 {
		if err := base.MsgPackDecode(c.PluginConfig, &config); err != nil {
			return err
		}
	}

	d.config = &config

	if c.AgentConfig != nil {
		d.nomadConfig = c.AgentConfig.Driver
	}

	wasmBin := path.Base(d.config.BinaryPath)
	supported := false
	for _, b := range supportedBins {
		if b == wasmBin {
			supported = true
		}
	}

	if !supported {
		return fmt.Errorf("invalid wasm binary: %s", wasmBin)
	}

	return nil
}

func (w *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (w *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) Fingerprint(_ context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(d.ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)

	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			// Immediately reset ticker
			ticker.Reset(fingerprintDuration)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	wasmBinary := d.config.BinaryPath
	//wasmBinaryName := path.Base(wasmBinary)

	_, err := os.Stat(wasmBinary)
	if err != nil {
		return &drivers.Fingerprint{
			Health:            drivers.HealthStateUndetected,
			HealthDescription: fmt.Sprintf("wasm exec binary %s not found", wasmBinary),
		}
	}

	fp.Attributes["driver.wasm_binary"] = structs.NewStringAttribute(wasmBinary)

	return fp
}

func (w *Driver) RecoverTask(_ *drivers.TaskHandle) error {
	panic("not implemented") // TODO: Implement
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	d.logger.Info("starting wasm task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	executorConfig := &executor.ExecutorConfig{
		LogFile:  filepath.Join(cfg.TaskDir().Dir, "executor.out"),
		LogLevel: "debug",
	}

	exec, pluginClient, err := executor.CreateExecutor(d.logger, d.nomadConfig, executorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create executor: %v", err)
	}

	execCmd := &executor.ExecCommand{
		Cmd:        d.config.BinaryPath,
		Args:       append([]string{driverConfig.Path}, driverConfig.Args...),
		StdoutPath: cfg.StdoutPath,
		StderrPath: cfg.StderrPath,
	}

	ps, err := exec.Launch(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to launch command with executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          ps.Pid,
		pluginClient: pluginClient,
		logger:       d.logger,
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
	}

	driverState := TaskState{
		ReattachConfig: structs.ReattachConfigFromGoPlugin(pluginClient.ReattachConfig()),
		Pid:            ps.Pid,
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	go h.run()

	d.tasks.Set(cfg.ID, h)

	return handle, nil, nil
}

func (w *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	panic("not implemented") // TODO: Implement
}

func (w *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	panic("not implemented") // TODO: Implement
}

func (w *Driver) DestroyTask(taskID string, force bool) error {
	panic("not implemented") // TODO: Implement
}

func (w *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	panic("not implemented") // TODO: Implement
}

func (w *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *cstructs.TaskResourceUsage, error) {
	panic("not implemented") // TODO: Implement
}

func (w *Driver) TaskEvents(_ context.Context) (<-chan *drivers.TaskEvent, error) {
	panic("not implemented") // TODO: Implement
}
