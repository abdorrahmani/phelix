package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func init() { SecurityCmd.AddCommand(newSupplyChainCommand()) }

func newSupplyChainCommand() *cobra.Command {
	root := &cobra.Command{Use: "supply-chain", Short: "Inspect versioned artifact security and enforce trust policies", Args: cobra.NoArgs}
	for _, action := range []string{"list", "show", "sbom", "provenance", "inventory", "licenses", "scan", "verify", "sign", "policy"} {
		var versionNumber, limit, offset int
		var digest, buildID, format, key, policyFile, mode string
		action := action
		cmd := &cobra.Command{Use: action + " [AppName]", Short: map[string]string{"list": "List artifact security history", "show": "Show a build's full security report", "sbom": "Export SPDX or CycloneDX JSON", "provenance": "Export SLSA provenance JSON", "inventory": "Show resolved direct and transitive dependencies", "licenses": "Show dependency license findings", "scan": "Rescan the exact recorded dependency inventory", "verify": "Verify artifact integrity, documents, signatures and policy", "sign": "Sign artifact, SBOMs and provenance with Cosign", "policy": "Inspect or update this application's security policy"}[action], Args: cobra.MaximumNArgs(1), SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveSecurityTarget(args)
			if err != nil {
				return err
			}
			appID := target.appID
			if appID == "" {
				appID = target.appName
			}
			store := supplychain.NewStore(server.DataDir())
			if action == "scan" || action == "verify" || action == "sign" || (action == "policy" && (policyFile != "" || mode != "")) {
				if err := authorizeDirectExecution(authz.ActionSecurity, target.appName, target.appID); err != nil {
					return err
				}
			}
			if action == "policy" {
				p, err := store.LoadPolicy(appID)
				if err != nil {
					return err
				}
				if policyFile != "" {
					var reader io.Reader
					var file *os.File
					if policyFile == "-" {
						reader = cmd.InOrStdin()
					} else {
						file, err = os.Open(policyFile)
						if err != nil {
							return err
						}
						defer file.Close()
						reader = file
					}
					raw, err := io.ReadAll(io.LimitReader(reader, 65537))
					if err != nil {
						return err
					}
					if len(raw) > 65536 {
						return errors.New("security policy exceeds size limit")
					}
					dec := json.NewDecoder(bytes.NewReader(raw))
					dec.DisallowUnknownFields()
					if err = dec.Decode(&p); err != nil {
						return err
					}
					if err = dec.Decode(new(any)); err != io.EOF {
						return errors.New("security policy contains trailing data")
					}
				}
				if mode != "" {
					p.Mode = mode
				}
				if policyFile != "" || mode != "" {
					if err = store.SavePolicy(appID, p); err != nil {
						return err
					}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
			}
			if action == "list" {
				if offset < 0 || limit < 1 || limit > 100 {
					return errors.New("limit must be 1-100 and offset cannot be negative")
				}
				reports, err := store.List(appID, limit, offset)
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(reports)
			}
			if versionNumber == 0 {
				reports, err := store.List(appID, 1, 0)
				if err != nil {
					return err
				}
				if len(reports) == 0 {
					return errors.New("no artifact security records exist; build this application first")
				}
				versionNumber = reports[0].Version
			}
			r, err := store.ForBuild(appID, versionNumber, digest, buildID)
			if err != nil {
				return err
			}
			switch action {
			case "sbom", "provenance":
				doc := r.Provenance
				if action == "sbom" {
					found := false
					for _, d := range r.SBOMs {
						if d.Format == format {
							doc = d
							found = true
							break
						}
					}
					if !found {
						return errors.New("format must be spdx-2.3 or cyclonedx-1.6")
					}
				}
				raw, err := store.Document(doc.SHA256)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(append(raw, '\n'))
				return err
			case "inventory":
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r.Inventory)
			case "licenses":
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r.Licenses)
			case "scan", "verify", "sign":
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
				defer cancel()
				if action == "scan" {
					err = supplychain.Rescan(ctx, store, r)
				} else {
					path, pathErr := supplyChainArtifactPath(target.appName, r)
					if pathErr != nil {
						return pathErr
					}
					if action == "verify" {
						err = supplychain.Verify(ctx, store, r, path)
					} else {
						err = supplychain.Sign(ctx, store, r, path, key)
					}
				}
				if saveErr := store.Save(r); saveErr != nil {
					return errors.Join(err, saveErr)
				}
				if renderErr := json.NewEncoder(cmd.OutOrStdout()).Encode(r); renderErr != nil {
					return errors.Join(err, renderErr)
				}
				return err
			default:
				return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
			}
		}}
		if action != "list" && action != "policy" {
			cmd.Flags().IntVar(&versionNumber, "version", 0, "Artifact version number; default latest")
			cmd.Flags().StringVar(&digest, "digest", "", "SHA-256 digest to select one matrix artifact")
			cmd.Flags().StringVar(&buildID, "build-id", "", "Matrix combination ID to select its independent build evidence")
		}
		if action == "list" {
			cmd.Flags().IntVar(&limit, "limit", 20, "History page size, 1-100")
			cmd.Flags().IntVar(&offset, "offset", 0, "History offset")
		}
		if action == "sbom" {
			cmd.Flags().StringVar(&format, "format", "cyclonedx-1.6", "spdx-2.3 or cyclonedx-1.6")
		}
		if action == "sign" {
			cmd.Flags().StringVar(&key, "key", "", "Cosign managed signing key reference; empty uses keyless identity")
		}
		if action == "policy" {
			cmd.Flags().StringVar(&policyFile, "file", "", "Update policy fields from JSON file; - reads stdin")
			cmd.Flags().StringVar(&mode, "mode", "", "disabled, audit or enforce")
		}
		root.AddCommand(cmd)
	}
	return root
}

func supplyChainArtifactPath(name string, r *supplychain.Report) (string, error) {
	versions, err := deploy.LoadVersions(name)
	if err != nil {
		return "", err
	}
	for _, v := range versions.Versions {
		if v.Version == r.Version {
			for _, a := range v.MatrixArtifacts {
				if a.SHA256 == r.Artifact.SHA256 && a.Binary != "" && (r.BuildID == "" || (a.Security != nil && a.Security.BuildID == r.BuildID)) {
					return a.Binary, nil
				}
			}
			if v.DockerImage != "" {
				return "", errors.New("native artifact verification does not cover container images")
			}
			dir, err := deploy.AppDataDir(name)
			if err != nil {
				return "", err
			}
			return filepath.Join(dir, "builds", "v"+strconv.Itoa(r.Version), "binary"), nil
		}
	}
	return "", fmt.Errorf("artifact version v%d is unavailable", r.Version)
}
