package project

// InitialConfig returns the defaults used when initializing a project.
func InitialConfig(name, workloadType string, publicPort int) *Config {
	cfg := &Config{
		Name: name, Port: publicPort, Watching: WatchingDisable,
		Health: &HealthConfig{Endpoints: []HealthEndpoint{{Name: "default", Path: "/health", Interval: "10s", Retries: 3, Mode: "auto"}}},
		Deploy: &DeployConfig{Strategy: StrategyClassic},
	}
	if workloadType == TypeWorker {
		cfg.Type = TypeWorker
		cfg.Port = 0
		cfg.Health = &HealthConfig{Mode: WorkerHealthProcess}
		cfg.Process = &ProcessConfig{}
		cfg.Deploy = &DeployConfig{Strategy: StrategyClassic, Replicas: 1}
	}
	if workloadType == TypeJob {
		cfg.Type = TypeJob
		cfg.Port = 0
		cfg.Health = nil
		cfg.Deploy = nil
		cfg.Schedule = &ScheduleConfig{Cron: "@daily", Timezone: DefaultJobScheduleTimezone}
		cfg.Job = &JobConfig{Command: []string{}}
	}
	return cfg
}
