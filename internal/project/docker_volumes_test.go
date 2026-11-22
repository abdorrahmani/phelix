package project

import "testing"

func TestDockerVolumeConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		volumes []DockerVolume
		valid   bool
	}{
		{"named volume", []DockerVolume{{Name: "b-phelix_phelix-attachments", Target: "/data/ticket-attachments"}}, true},
		{"invalid name", []DockerVolume{{Name: "../attachments", Target: "/data"}}, false},
		{"relative target", []DockerVolume{{Name: "attachments", Target: "data"}}, false},
		{"root target", []DockerVolume{{Name: "attachments", Target: "/"}}, false},
		{"unclean target", []DockerVolume{{Name: "attachments", Target: "/data/../etc"}}, false},
		{"mount injection", []DockerVolume{{Name: "attachments", Target: "/data,readonly"}}, false},
		{"duplicate target", []DockerVolume{{Name: "first", Target: "/data"}, {Name: "second", Target: "/data"}}, false},
		{"too many volumes", make([]DockerVolume, 17), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Deploy: &DeployConfig{Runtime: RuntimeDocker, Strategy: StrategyBlueGreen, Docker: &DockerRuntimeConfig{Volumes: test.volumes}}}
			if err := cfg.validate(); (err == nil) != test.valid {
				t.Fatalf("valid = %t, error = %v", test.valid, err)
			}
		})
	}
}
