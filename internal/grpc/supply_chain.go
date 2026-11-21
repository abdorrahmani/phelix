package grpc

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

const (
	CapabilitySupplyChain   = "supply_chain_v1"
	SecurityArtifacts       = "security_artifacts"
	SecurityArtifactDetail  = "security_artifact_detail"
	SecurityArtifactRescan  = "security_artifact_rescan"
	SecurityArtifactVerify  = "security_artifact_verify"
	SecuritySBOM            = "security_sbom"
	SecurityProvenance      = "security_provenance"
	SecurityPolicy          = "security_policy"
	SecurityPolicySet       = "security_policy_set"
	SecurityRemoteStatus    = "security_remote_status"
	SecurityRemoteEnable    = "security_remote_enable"
	SecurityRemoteDisable   = "security_remote_disable"
	SecurityOperationCancel = "security_operation_cancel"
	SecurityAppStatus       = "security_app_status"
	SecurityAppLogs         = "security_app_logs"
	SecurityAppBuild        = "security_app_build"
)

func init() { RegisterCapability(CapabilitySupplyChain) }

func IsSupplyChainCommand(command string) bool {
	return slices.Contains([]string{SecurityArtifacts, SecurityArtifactDetail, SecurityArtifactRescan, SecurityArtifactVerify,
		SecuritySBOM, SecurityProvenance, SecurityPolicy, SecurityPolicySet, SecurityRemoteStatus, SecurityRemoteEnable,
		SecurityRemoteDisable, SecurityOperationCancel, SecurityAppStatus, SecurityAppLogs, SecurityAppBuild}, command)
}

func ValidateSupplyChainCommand(req *pb.MonitorCommandRequest) error {
	opts := req.GetSecurity()
	if opts == nil {
		opts = &pb.SecurityCommandOptions{}
	}
	if opts.GetProtocolVersion() > SecurityProtocolVersion || opts.GetAdvisoryId() != "" || opts.GetFindingsOffset() != 0 ||
		opts.GetSeverity() != "" || opts.GetEcosystem() != "" || opts.GetState() != "" || opts.GetPackage() != "" ||
		opts.GetIncludeGraph() || opts.GetSetEnabled() || opts.GetEnabled() || opts.GetScanInterval() != "" || opts.GetFailThreshold() != "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "supply chain command contains unrelated options")
	}
	if req.GetType() != SecurityOperationCancel && opts.GetScanId() != "" ||
		req.GetType() != SecurityPolicySet && opts.GetPolicyJson() != "" ||
		req.GetType() != SecuritySBOM && opts.GetFormat() != "" ||
		req.GetType() != SecurityArtifacts && (opts.GetLimit() != 0 || opts.GetOffset() != 0) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "supply chain options do not match the operation")
	}
	if opts.GetArtifactDigest() != "" {
		if opts.GetVersion() <= 0 || len(opts.GetArtifactDigest()) != 64 || opts.GetArtifactDigest() != strings.ToLower(opts.GetArtifactDigest()) {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "artifact digest requires a version and a SHA-256 digest")
		}
		if _, err := hex.DecodeString(opts.GetArtifactDigest()); err != nil {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "artifact digest is malformed")
		}
	}
	if opts.GetBuildId() != "" && (opts.GetVersion() <= 0 || !regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`).MatchString(opts.GetBuildId())) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "build identifier requires a version and a valid matrix combination")
	}
	switch req.GetType() {
	case SecurityArtifactDetail, SecurityArtifactRescan, SecurityArtifactVerify, SecuritySBOM, SecurityProvenance:
		if opts.GetVersion() <= 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "an existing positive artifact version is required")
		}
	default:
		if opts.GetVersion() != 0 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "version is not valid for this operation")
		}
	}
	if opts.GetOffset() < 0 || opts.GetLimit() < 0 || opts.GetLimit() > 100 {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "invalid security page bounds")
	}
	if req.GetType() == SecuritySBOM && !slices.Contains([]string{"spdx", "cyclonedx", "spdx-2.3", "cyclonedx-1.6"}, opts.GetFormat()) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "SBOM format must be spdx-2.3 or cyclonedx-1.6")
	}
	if req.GetType() == SecurityPolicySet {
		if len(opts.GetPolicyJson()) == 0 || len(opts.GetPolicyJson()) > 16384 {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "a bounded security policy is required")
		}
		var policy supplychain.Policy
		decoder := json.NewDecoder(strings.NewReader(opts.GetPolicyJson()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security policy is malformed")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "security policy contains trailing data")
		}
		if err := supplychain.ValidatePolicy(policy); err != nil {
			return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid security policy", err)
		}
	}
	if req.GetType() == SecurityOperationCancel && (opts.GetScanId() == "" || len(opts.GetScanId()) > 64 || strings.ContainsAny(opts.GetScanId(), "/\\\r\n")) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "a valid operation identifier is required")
	}
	return nil
}

func (c *Client) sendSupplyChainSnapshots() {
	state, err := security.LoadRemoteState(server.DataDir())
	if err != nil {
		logs.ErrorFile("security", "remote security state unavailable: %v", err)
		return
	}
	if state.Revision == 0 {
		state, err = security.SetRemoteSecurity(server.DataDir(), false)
		if err != nil {
			logs.ErrorFile("security", "remote security initialization failed: %v", err)
			return
		}
	}
	if state.Enabled {
		if err := c.sendMonitorServerInfo(); err != nil {
			logs.ErrorFile("security", "remote security server registration failed: %v", err)
			return
		}
	}
	stateRaw, err := json.Marshal(map[string]any{"remote": state})
	if err != nil {
		logs.ErrorFile("security", "remote security state encoding failed: %v", err)
		return
	}
	if err := monitorStream.send(&pb.MonitorEvent{ServerId: server.GetAgentID(), Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: stateRaw}}); err != nil {
		logs.ErrorFile("security", "remote security synchronization failed: %v", err)
		return
	}
	if !state.Enabled {
		return
	}
	apps := app.Manager.ListApplications()
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	if len(apps) == 0 {
		return
	}
	c.supplyChainAppOffset %= len(apps)
	info := apps[c.supplyChainAppOffset]
	store := supplychain.NewStore(server.DataDir())
	reports, err := store.List(info.ID, 1, c.supplyChainReportOffset)
	if err != nil {
		logs.ErrorFile("security", "artifact security synchronization failed: %v", err)
		return
	}
	if len(reports) == 0 {
		c.supplyChainReportOffset = 0
		c.supplyChainAppOffset = (c.supplyChainAppOffset + 1) % len(apps)
		return
	}
	report := reports[0]
	report.Documents = make(map[string][]byte)
	refs := append(append([]supplychain.Document{}, report.SBOMs...), report.Provenance)
	refs = append(refs, report.Signature.Bundles...)
	for _, doc := range refs {
		if doc.SHA256 == "" {
			continue
		}
		raw, err := store.Document(doc.SHA256)
		if err != nil {
			logs.ErrorFile("security", "artifact document synchronization failed: %v", err)
			return
		}
		report.Documents[doc.SHA256] = raw
	}
	raw, err := json.Marshal(map[string]any{"report": report, "remote": state})
	if err != nil {
		logs.ErrorFile("security", "artifact report encoding failed: %v", err)
		return
	}
	if !security.RemoteSecurityEnabled(server.DataDir()) {
		return
	}
	if err := c.sendSupplyChainJSON(raw, report.AppID); err != nil {
		logs.ErrorFile("security", "artifact report synchronization failed: %v", err)
		return
	}
	c.supplyChainReportOffset++
}

func (c *Client) sendSupplyChainJSON(raw []byte, appID string) error {
	send := func(payload []byte) error {
		if !supplyChainReadEnabled() {
			return phelixerr.New(phelixerr.CodeSecurityReportingDisabled, "remote security was disabled during artifact synchronization")
		}
		return monitorStream.send(&pb.MonitorEvent{ServerId: server.GetAgentID(), Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: payload}})
	}
	if len(raw) <= 3<<20 {
		return send(raw)
	}
	if len(raw) > 64<<20 {
		return phelixerr.New(phelixerr.CodeSecurityResponseTooLarge, "artifact report exceeds the 64 MiB synchronization limit")
	}
	const chunkSize = 512 << 10
	parts := (len(raw) + chunkSize - 1) / chunkSize
	id := supplychain.Digest(raw)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for part := 0; part < parts; part++ {
		if part > 0 {
			select {
			case <-ticker.C:
			case <-c.done:
				return phelixerr.New(phelixerr.CodeUnavailable, "controller stopped during artifact synchronization")
			}
		}
		payload, err := json.Marshal(map[string]any{"transfer": map[string]any{"id": id, "app_id": appID, "part": part, "parts": parts, "payload": raw[part*chunkSize : min(len(raw), (part+1)*chunkSize)]}})
		if err != nil {
			return err
		}
		if err := send(payload); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) queueSupplyChainSnapshots() {
	if !c.supplyChainMu.TryLock() {
		return
	}
	select {
	case <-c.done:
		c.supplyChainMu.Unlock()
		return
	default:
	}
	go func() {
		defer c.supplyChainMu.Unlock()
		c.sendSupplyChainSnapshots()
	}()
}

func acknowledgeSupplyChain(ack *pb.SupplyChainAck) {
	if ack.GetAppId() == "" && ack.GetDigest() == "" {
		if err := security.AcknowledgeRemoteSecurity(server.DataDir(), ack.GetRevision(), phelixerr.Redact(ack.GetError())); err != nil {
			logs.ErrorFile("security", "remote security acknowledgment persistence failed: %v", err)
		}
		return
	}
	if ack.GetError() != "" {
		logs.ErrorFile("security", "artifact security synchronization rejected: %s", phelixerr.Redact(ack.GetError()))
	}
}

func supplyChainReadEnabled() bool {
	return security.RemoteSecurityEnabled(server.DataDir())
}
