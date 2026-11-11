package grpc

import (
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func jobReq(requestID, cmdType string, mutate func(*pb.MonitorCommandRequest)) *pb.MonitorCommandRequest {
	req := &pb.MonitorCommandRequest{RequestId: requestID, Type: cmdType, AppName: "nightly"}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func TestValidateJobCommand_AcceptsEveryShape(t *testing.T) {
	valid := map[string]*pb.MonitorCommandRequest{
		"list": jobReq("r", JobCommandList, nil),
		"list name": jobReq("r", JobCommandList, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{JobName: "nightly"}
		}),
		"run": jobReq("r", JobCommandRun, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{JobName: "nightly"}
		}),
		"history": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Limit: 5}
		}),
		"history one": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef"}
		}),
		"logs": jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef", Offset: 10, MaxBytes: 1024}
		}),
	}
	for name, req := range valid {
		if err := validateJobCommand(req); err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
	}

	for cmdType := range map[string]bool{JobCommandList: true, JobCommandRun: true, JobCommandHistory: true, JobCommandLogs: true} {
		if !isJobCommand(cmdType) {
			t.Fatalf("%s must dispatch as a job command", cmdType)
		}
	}
	if isJobCommand("restart") {
		t.Fatal("generic restart must not dispatch through the job ledger")
	}
	if !isMutatingJobCommand(JobCommandRun) {
		t.Fatal("job_run must be ledger-guarded")
	}
	for _, cmdType := range []string{JobCommandList, JobCommandHistory, JobCommandLogs} {
		if isMutatingJobCommand(cmdType) {
			t.Fatalf("%s must be read-only", cmdType)
		}
	}
}

func TestValidateJobCommand_RejectsUnexpectedShape(t *testing.T) {
	cases := map[string]*pb.MonitorCommandRequest{
		"missing request id": jobReq("", JobCommandList, nil),
		"missing app": func() *pb.MonitorCommandRequest {
			req := jobReq("r", JobCommandList, nil)
			req.AppName = ""
			return req
		}(),
		"unknown type": jobReq("r", "job_nuke", nil),
		"foreign strategy": func() *pb.MonitorCommandRequest {
			req := jobReq("r", JobCommandList, nil)
			req.Strategy = "blue-green"
			return req
		}(),
		"foreign matrix": func() *pb.MonitorCommandRequest {
			req := jobReq("r", JobCommandRun, nil)
			req.Matrix = &pb.MatrixOptions{}
			return req
		}(),
		"foreign limit": func() *pb.MonitorCommandRequest {
			req := jobReq("r", JobCommandList, nil)
			req.Limit = 5
			return req
		}(),
		"limit on run": jobReq("r", JobCommandRun, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Limit: 5}
		}),
		"limit on list": jobReq("r", JobCommandList, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Limit: 5}
		}),
		"execution on list": jobReq("r", JobCommandList, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef"}
		}),
		"offset on history": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Offset: 1}
		}),
		"max bytes on history": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{MaxBytes: 1}
		}),
		"missing logs execution": jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Offset: 1}
		}),
		"logs without options": jobReq("r", JobCommandLogs, nil),
		"name on logs": jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef", JobName: "nightly"}
		}),
		"limit on logs": jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef", Limit: 5}
		}),
		"negative limit": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Limit: -1}
		}),
		"limit too large": jobReq("r", JobCommandHistory, func(r *pb.MonitorCommandRequest) {
			r.Job = &pb.JobOptions{Limit: JobHistoryLimitMax + 1}
		}),
		"negative offset": func() *pb.MonitorCommandRequest {
			req := jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
				r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef", Offset: -1}
			})
			return req
		}(),
		"max bytes too large": func() *pb.MonitorCommandRequest {
			return jobReq("r", JobCommandLogs, func(r *pb.MonitorCommandRequest) {
				r.Job = &pb.JobOptions{ExecutionId: "job_0123456789abcdef", MaxBytes: JobLogBytesMax + 1}
			})
		}(),
	}
	for name, req := range cases {
		if err := validateJobCommand(req); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
}
