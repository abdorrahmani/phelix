package grpc

import (
	"strings"
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func TestSupplyChainCommandValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		opts    *pb.SecurityCommandOptions
		valid   bool
	}{
		{"status", SecurityRemoteStatus, nil, true},
		{"detail", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: strings.Repeat("a", 64)}, true},
		{"matrix detail", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: strings.Repeat("a", 64), BuildId: "go1.27-linux-amd64"}, true},
		{"matrix traversal", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, BuildId: "../artifact"}, false},
		{"matrix without version", SecurityArtifacts, &pb.SecurityCommandOptions{BuildId: "go1.27-linux-amd64"}, false},
		{"missing version", SecurityArtifactDetail, nil, false},
		{"wrong digest", SecurityArtifactDetail, &pb.SecurityCommandOptions{Version: 3, ArtifactDigest: "../file"}, false},
		{"foreign scan fields", SecurityAppStatus, &pb.SecurityCommandOptions{SetEnabled: true}, false},
		{"negative offset", SecurityArtifacts, &pb.SecurityCommandOptions{Offset: -1}, false},
		{"format", SecuritySBOM, &pb.SecurityCommandOptions{Version: 3, Format: "spdx"}, true},
		{"unknown format", SecuritySBOM, &pb.SecurityCommandOptions{Version: 3, Format: "text"}, false},
		{"cancel", SecurityOperationCancel, &pb.SecurityCommandOptions{ScanId: "operation123"}, true},
		{"cancel traversal", SecurityOperationCancel, &pb.SecurityCommandOptions{ScanId: "../operation"}, false},
		{"policy unknown", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit","unknown":true}`}, false},
		{"policy trailing object", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit"} {"mode":"disabled"}`}, false},
		{"policy trailing garbage", SecurityPolicySet, &pb.SecurityCommandOptions{PolicyJson: `{"mode":"audit"} invalid`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &pb.MonitorCommandRequest{RequestId: "request", Type: tc.command, AppName: "app", Security: tc.opts}
			err := validateSecurityCommand(req)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
