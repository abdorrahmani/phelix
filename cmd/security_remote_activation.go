package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/spf13/cobra"
)

func init() {
	SecurityCmd.AddCommand(newRemoteSecurityCommand())
}

func newRemoteSecurityCommand() *cobra.Command {
	root := &cobra.Command{Use: "remote", Short: "Manage this server's remote security reporting", Args: cobra.NoArgs}
	root.PersistentFlags().BoolVar(&securityJSON, "json", false, "emit remote security state or result as JSON")
	for _, operation := range []string{"enable", "disable", "status", "config"} {
		root.AddCommand(&cobra.Command{
			Use: operation, Short: operation + " remote security reporting", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				state, err := security.LoadRemoteState(server.DataDir())
				if err != nil {
					return err
				}
				if operation == "enable" || operation == "disable" {
					session, err := auth.GetValidSession()
					if err != nil {
						return err
					}
					if session.Scope != "" && session.Scope != auth.ScopeFull {
						return phelixerr.New(phelixerr.CodePermissionDenied, "remote security activation requires a full account session")
					}
					id, err := server.EnsureAgentID()
					if err != nil {
						return err
					}
					if err := authorizeDirectExecution(authz.ActionHostSecurity, id, id); err != nil {
						return err
					}
					state, err = security.SetRemoteSecurity(server.DataDir(), operation == "enable")
					if err != nil {
						return err
					}
				}
				if securityJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Remote security: %s\nSynchronization: %s\n", boolString(state.Enabled), state.SyncStatus)
				if state.SyncStatus == "pending" {
					fmt.Fprintln(cmd.OutOrStdout(), "State is saved locally. Synchronization needs a connected Phelix controller.")
				}
				if state.SyncError != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Synchronization failed: %s\n", phelixerr.Redact(state.SyncError))
				}
				return nil
			},
		})
	}
	for _, operation := range []string{"scan", "report"} {
		root.AddCommand(&cobra.Command{
			Use: operation + " <AppName>", Short: operation + " security on this controller with remote reporting enabled", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				target, err := remoteSecurityTarget(args[0])
				if err != nil {
					return err
				}
				command := phelixgrpc.SecurityCommandReport
				if operation == "scan" {
					session, err := auth.GetValidSession()
					if err != nil {
						return err
					}
					if session.Scope != "" && session.Scope != auth.ScopeFull {
						return phelixerr.New(phelixerr.CodePermissionDenied, "remote security scanning requires a full account session")
					}
					if err := authorizeDirectExecution(authz.ActionSecurity, target.appName, target.appID); err != nil {
						return err
					}
					command = phelixgrpc.SecurityCommandRun
				}
				req := &pb.MonitorCommandRequest{RequestId: security.NewScanID(), Type: command, AppName: target.appID}
				if err := remoteSecurityConsent(target); err != nil {
					return err
				}
				var result *pb.MonitorCommandResult
				if operation == "scan" {
					result = remoteSecurityScanRunContext(cmd.Context(), req, target)
				} else {
					result = remoteSecurityReport(req, target)
				}
				if result.GetStatus() != "success" {
					return phelixerr.New(phelixerr.Code(result.GetErrorCode()), result.GetError())
				}
				if securityJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(result.GetSecurityScanRecord())
				}
				record := result.GetSecurityScanRecord()
				fmt.Fprintf(cmd.OutOrStdout(), "Scan: %s\nStatus: %s\nCoverage: %s\nAffected dependencies: %d\n", record.GetScanId(), record.GetStatus(), record.GetCoverage(), record.GetCounts().GetAffected())
				return nil
			},
		})
	}
	return root
}
