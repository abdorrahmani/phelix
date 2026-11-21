package supplychain

import (
	"encoding/json"
	"fmt"
	"github.com/abdorrahmani/phelix/internal/security"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func packageID(key string) string { return "SPDXRef-" + Digest([]byte(key))[:24] }

func packageURL(eco security.Ecosystem, m security.Module) string {
	typ := "golang"
	if eco == security.EcosystemRust {
		typ = "cargo"
	}
	return "pkg:" + typ + "/" + strings.ReplaceAll(url.PathEscape(m.Name), "%2F", "/") + "@" + url.PathEscape(m.Version)
}

func licenseFor(r *Report, name, version string) string {
	for _, l := range r.Licenses {
		if l.Package == name && l.Version == version && l.Expression != "" {
			return l.Expression
		}
	}
	return "NOASSERTION"
}

func GenerateDocuments(s *Store, r *Report) error {
	spdx, err := SPDX(r)
	if err != nil {
		return err
	}
	cyclone, err := CycloneDX(r)
	if err != nil {
		return err
	}
	sd, err := s.PutDocument("spdx-2.3", spdx)
	if err != nil {
		return err
	}
	cd, err := s.PutDocument("cyclonedx-1.6", cyclone)
	if err != nil {
		return err
	}
	r.SBOMs = []Document{sd, cd}
	prov, err := Provenance(r)
	if err != nil {
		return err
	}
	r.Provenance, err = s.PutDocument("slsa-provenance-v1", prov)
	return err
}

func SPDX(r *Report) ([]byte, error) {
	root := packageID("application")
	packages := []any{map[string]any{"SPDXID": root, "name": r.AppName, "versionInfo": strconv.Itoa(r.Version), "downloadLocation": "NOASSERTION", "filesAnalyzed": false, "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION", "checksums": []any{map[string]any{"algorithm": "SHA256", "checksumValue": r.Artifact.SHA256}}}}
	buildMetadata, err := json.Marshal(map[string]any{"source": r.Source, "builder": r.Builder, "buildId": r.BuildID, "targetOS": r.Artifact.TargetOS, "targetArchitecture": r.Artifact.TargetArch})
	if err != nil {
		return nil, err
	}
	packages[0].(map[string]any)["sourceInfo"] = string(buildMetadata)
	relationships := []any{map[string]any{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": root}}
	known := map[string]bool{}
	for _, key := range r.Inventory.Roots {
		known[key] = true
	}
	for _, m := range sortedModules(r.Inventory) {
		known[m.Key] = true
		p := map[string]any{"SPDXID": packageID(m.Key), "name": m.Name, "versionInfo": m.Version, "downloadLocation": "NOASSERTION", "filesAnalyzed": false, "licenseConcluded": "NOASSERTION", "licenseDeclared": licenseFor(r, m.Name, m.Version), "copyrightText": "NOASSERTION"}
		if m.SourceDetail != "" {
			p["sourceInfo"] = m.SourceDetail
		}
		if m.Version != "" {
			p["externalRefs"] = []any{map[string]any{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": packageURL(r.Inventory.Ecosystem, m)}}
		}
		if hash := r.PackageHashes[m.Key]; hash != "" {
			p["checksums"] = []any{map[string]any{"algorithm": "SHA256", "checksumValue": hash}}
		}
		packages = append(packages, p)
		if m.Direct {
			relationships = append(relationships, map[string]any{"spdxElementId": root, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": packageID(m.Key)})
		}
	}
	rootKeys := map[string]bool{}
	for _, key := range r.Inventory.Roots {
		rootKeys[key] = true
	}
	seen := map[string]bool{}
	for _, edge := range r.Inventory.Edges {
		if !known[edge.From] || !known[edge.To] {
			continue
		}
		from := packageID(edge.From)
		if rootKeys[edge.From] {
			from = root
		}
		to := packageID(edge.To)
		if rootKeys[edge.To] {
			to = root
		}
		key := from + ":" + to
		if seen[key] {
			continue
		}
		seen[key] = true
		relationships = append(relationships, map[string]any{"spdxElementId": from, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": to})
	}
	doc := map[string]any{"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT", "name": r.AppName + "-v" + strconv.Itoa(r.Version), "documentNamespace": "https://phelix.dev/spdx/" + url.PathEscape(r.AppID) + "/" + Reference(r), "creationInfo": map[string]any{"creators": []string{"Tool: Phelix-" + r.Builder.PhelixVersion}, "created": r.BuiltAt.UTC().Format(time.RFC3339)}, "packages": packages, "relationships": relationships}
	return json.Marshal(doc)
}

func CycloneDX(r *Report) ([]byte, error) {
	root := "application:" + r.Artifact.SHA256
	components := []any{}
	deps := map[string][]string{root: {}}
	rootKeys := map[string]bool{}
	for _, key := range r.Inventory.Roots {
		rootKeys[key] = true
	}
	for _, m := range sortedModules(r.Inventory) {
		component := map[string]any{"type": "library", "bom-ref": m.Key, "name": m.Name, "version": m.Version, "properties": []any{map[string]any{"name": "phelix:direct", "value": strconv.FormatBool(m.Direct)}, map[string]any{"name": "phelix:ecosystem", "value": string(r.Inventory.Ecosystem)}, map[string]any{"name": "phelix:confidence", "value": string(m.Confidence)}}}
		if m.SourceDetail != "" {
			component["properties"] = append(component["properties"].([]any), map[string]any{"name": "phelix:source", "value": m.SourceDetail})
		}
		if m.Version != "" {
			component["purl"] = packageURL(r.Inventory.Ecosystem, m)
		}
		if license := licenseFor(r, m.Name, m.Version); license != "NOASSERTION" {
			component["licenses"] = []any{map[string]any{"expression": license}}
		}
		if hash := r.PackageHashes[m.Key]; hash != "" {
			component["hashes"] = []any{map[string]any{"alg": "SHA-256", "content": hash}}
		}
		components = append(components, component)
		deps[m.Key] = []string{}
		if m.Direct && !contains(deps[root], m.Key) {
			deps[root] = append(deps[root], m.Key)
		}
	}
	for _, edge := range r.Inventory.Edges {
		from, to := edge.From, edge.To
		if rootKeys[from] {
			from = root
		}
		if rootKeys[to] {
			to = root
		}
		if _, ok := deps[from]; !ok {
			continue
		}
		if _, ok := deps[to]; !ok {
			continue
		}
		if !contains(deps[from], to) {
			deps[from] = append(deps[from], to)
		}
	}
	keys := []string{root}
	for _, m := range sortedModules(r.Inventory) {
		keys = append(keys, m.Key)
	}
	dependencies := []any{}
	for _, key := range keys {
		dependencies = append(dependencies, map[string]any{"ref": key, "dependsOn": deps[key]})
	}
	documentID := Digest([]byte(r.AppID + "/" + Reference(r)))
	uuid := documentID[:8] + "-" + documentID[8:12] + "-4" + documentID[13:16] + "-8" + documentID[17:20] + "-" + documentID[20:32]
	metadata := map[string]any{"timestamp": r.BuiltAt.UTC().Format(time.RFC3339), "component": map[string]any{"type": "application", "bom-ref": root, "name": r.AppName, "version": strconv.Itoa(r.Version), "hashes": []any{map[string]any{"alg": "SHA-256", "content": r.Artifact.SHA256}}}, "properties": []any{map[string]any{"name": "phelix:target-os", "value": r.Artifact.TargetOS}, map[string]any{"name": "phelix:target-architecture", "value": r.Artifact.TargetArch}, map[string]any{"name": "phelix:compiler", "value": r.Builder.CompilerVersion}}}
	for _, field := range [][2]string{{"build-id", r.BuildID}, {"source-repository", r.Source.Repository}, {"source-commit", r.Source.Commit}, {"source-ref", r.Source.Ref}, {"source-sha256", r.Source.SHA256}, {"language", r.Builder.Language}, {"phelix-version", r.Builder.PhelixVersion}} {
		if field[1] != "" {
			metadata["properties"] = append(metadata["properties"].([]any), map[string]any{"name": "phelix:" + field[0], "value": field[1]})
		}
	}
	doc := map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "serialNumber": "urn:uuid:" + uuid, "version": 1, "metadata": metadata, "components": components, "dependencies": dependencies}
	return json.Marshal(doc)
}

func Provenance(r *Report) ([]byte, error) {
	dependencies := []any{}
	if r.Source.SHA256 != "" {
		dependencies = append(dependencies, map[string]any{"uri": "urn:phelix:source-tree", "digest": map[string]string{"sha256": r.Source.SHA256}})
	}

	if r.Source.Repository != "" && r.Source.Commit != "" {
		dependencies = append(dependencies, map[string]any{"uri": r.Source.Repository, "digest": map[string]string{"gitCommit": r.Source.Commit}})
	}
	for _, d := range r.SBOMs {
		dependencies = append(dependencies, map[string]any{"uri": "urn:phelix:sbom:" + d.Format, "digest": map[string]string{"sha256": d.SHA256}})
	}
	predicate := map[string]any{"buildDefinition": map[string]any{"buildType": "https://phelix.dev/build/native/v1", "externalParameters": map[string]any{"application": r.AppName, "applicationId": r.AppID, "version": r.Version, "language": r.Builder.Language, "arguments": r.Builder.Arguments, "targetOS": r.Artifact.TargetOS, "targetArchitecture": r.Artifact.TargetArch, "sourceRef": r.Source.Ref}, "internalParameters": map[string]any{"compiler": r.Builder.Compiler, "compilerVersion": r.Builder.CompilerVersion, "phelixVersion": r.Builder.PhelixVersion, "sourceDirty": r.Source.Dirty}, "resolvedDependencies": dependencies}, "runDetails": map[string]any{"builder": map[string]any{"id": r.Builder.ID}, "metadata": map[string]any{"invocationId": fmt.Sprintf("%s/v%d/%s", r.AppID, r.Version, r.Artifact.SHA256), "finishedOn": r.BuiltAt.UTC().Format(time.RFC3339)}}}
	if r.BuildID != "" {
		predicate["buildDefinition"].(map[string]any)["externalParameters"].(map[string]any)["buildId"] = r.BuildID
		predicate["runDetails"].(map[string]any)["metadata"].(map[string]any)["invocationId"] = r.AppID + "/" + Reference(r)
	}
	predicate["runDetails"].(map[string]any)["byproducts"] = []any{map[string]any{"name": r.Artifact.Filename, "digest": map[string]string{"sha256": r.Artifact.SHA256}, "annotations": map[string]any{"sizeBytes": r.Artifact.SizeBytes, "buildResult": "success"}}}
	return json.Marshal(map[string]any{"_type": "https://in-toto.io/Statement/v1", "subject": []any{map[string]any{"name": r.Artifact.Filename, "digest": map[string]string{"sha256": r.Artifact.SHA256}}}, "predicateType": "https://slsa.dev/provenance/v1", "predicate": predicate})
}

func VerifyDocuments(s *Store, r *Report) error {
	for _, d := range r.SBOMs {
		raw, err := s.Document(d.SHA256)
		if err != nil {
			return err
		}
		if int64(len(raw)) != d.SizeBytes {
			return fmt.Errorf("%s document size differs", d.Format)
		}
		expected, err := SPDX(r)
		if d.Format == "cyclonedx-1.6" {
			expected, err = CycloneDX(r)
		} else if d.Format != "spdx-2.3" {
			return errorsNewFormat(d.Format)
		}
		if err != nil {
			return err
		}
		if Digest(expected) != d.SHA256 {
			return fmt.Errorf("%s document is associated with a different artifact or inventory", d.Format)
		}
	}
	if r.Provenance.SHA256 != "" {
		raw, err := s.Document(r.Provenance.SHA256)
		if err != nil {
			return err
		}
		expected, err := Provenance(r)
		if err != nil {
			return err
		}
		if Digest(expected) != r.Provenance.SHA256 || int64(len(raw)) != r.Provenance.SizeBytes {
			return fmt.Errorf("provenance does not match artifact, version and SBOM digests")
		}
	}
	return nil
}

func errorsNewFormat(format string) error { return fmt.Errorf("unsupported SBOM format %q", format) }
