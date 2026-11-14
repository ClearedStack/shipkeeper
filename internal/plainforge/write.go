package plainforge

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/manifest"
	"github.com/ClearedStack/shipkeeper/internal/registry"
)

const (
	maxPushAttempts = 3
	skipCIMarker    = "[skip-ci]"
	gitAuthorIdent  = "Shipkeeper <shipkeeper@clearedstack.internal>"
)

var errPushRejected = errors.New("plainforge ref update rejected")

// UpdateAppYAML returns a copy of src with its top-level digest and/or
// replicas lines updated while preserving comments, blank lines, and the
// ordering of all other fields. Both the input and output are validated with
// manifest.Parse.
func UpdateAppYAML(src []byte, newDigest string, newReplicas *int) ([]byte, bool, error) {
	if _, err := manifest.Parse(src); err != nil {
		return nil, false, err
	}
	if newDigest == "" && newReplicas == nil {
		cp := make([]byte, len(src))
		copy(cp, src)
		return cp, false, nil
	}

	text := string(src)
	trailingNL := strings.HasSuffix(text, "\n")
	if trailingNL {
		text = strings.TrimSuffix(text, "\n")
	}
	lines := strings.Split(text, "\n")

	replacedDigest := false
	replacedReplicas := false
	for i, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "digest":
			if newDigest != "" {
				lines[i] = "digest: " + newDigest + inlineComment(rest)
				replacedDigest = true
			}
		case "replicas":
			if newReplicas != nil {
				lines[i] = fmt.Sprintf("replicas: %d%s", *newReplicas, inlineComment(rest))
				replacedReplicas = true
			}
		}
	}
	if newDigest != "" && !replacedDigest {
		return nil, false, errs.ManifestMissing
	}
	if newReplicas != nil && !replacedReplicas {
		return nil, false, errs.ManifestMissing
	}

	outStr := strings.Join(lines, "\n")
	if trailingNL {
		outStr += "\n"
	}
	out := []byte(outStr)
	parsed, err := manifest.Parse(out)
	if err != nil {
		return nil, false, err
	}
	if newDigest != "" && parsed.Digest != newDigest {
		return nil, false, errs.ManifestValue
	}
	if newReplicas != nil && parsed.Replicas != *newReplicas {
		return nil, false, errs.ManifestValue
	}
	return out, !bytes.Equal(src, out), nil
}

func inlineComment(rest string) string {
	if idx := strings.Index(rest, " #"); idx >= 0 {
		return rest[idx:]
	}
	if idx := strings.Index(rest, "\t#"); idx >= 0 {
		return rest[idx:]
	}
	return ""
}

// UpdateAppManifest commits updated app.yaml bytes to the owning repository's
// main branch on Plainforge over Git smart HTTP (git-receive-pack), always
// tagging the commit message with [skip-ci] so Keelworks does not trigger a
// rebuild loop.
func (c *Client) UpdateAppManifest(ctx context.Context, app string, data []byte, summary string) error {
	if c.BaseURL == "" || c.HTTP == nil {
		return errs.Config
	}
	if len(data) > maxAppBytes {
		return errs.YAMLUnsupported
	}
	if _, err := manifest.Parse(data); err != nil {
		return err
	}
	repo := app
	if comp, ok := registry.Lookup(app); ok {
		repo = comp.Repository
	}
	if !repoRE.MatchString(repo) || !repoRE.MatchString(app) {
		return errs.Config
	}
	msg, err := cleanCommitMessage(app, summary)
	if err != nil {
		return err
	}

	ref := c.Ref
	if ref == "" {
		ref = defaultRef
	}

	for attempt := 0; attempt < maxPushAttempts; attempt++ {
		tipOID, caps, err := c.fetchServiceRef(ctx, repo, "git-receive-pack", ref)
		if err != nil {
			return err
		}
		if !containsToken(caps, "report-status") {
			return errs.PlainforgeBadReply
		}

		objs, err := c.fetchObjectsAt(ctx, repo, tipOID)
		if err != nil {
			return err
		}
		commit, ok := objs[tipOID]
		if !ok || commit.kind != kindCommit {
			return errs.PlainforgeBadReply
		}

		newRootTreeOID, newObjs, err := buildUpdatedTreeObjects(objs, commit.data, app, data)
		if err != nil {
			return err
		}
		oldRootTreeOID := parseHeaderValue(commit.data, "tree ")
		if newRootTreeOID == oldRootTreeOID {
			c.invalidateCache(repo)
			return nil
		}

		ts := time.Now().UTC().Unix()
		if parentTS := parseCommitterTimestamp(commit.data); parentTS > ts {
			ts = parentTS
		}
		var commitBuf bytes.Buffer
		fmt.Fprintf(&commitBuf, "tree %s\n", newRootTreeOID)
		fmt.Fprintf(&commitBuf, "parent %s\n", tipOID)
		fmt.Fprintf(&commitBuf, "author %s %d +0000\n", gitAuthorIdent, ts)
		fmt.Fprintf(&commitBuf, "committer %s %d +0000\n\n", gitAuthorIdent, ts)
		commitBuf.WriteString(msg)

		commitObj := gitObject{kind: kindCommit, data: commitBuf.Bytes()}
		newCommitOID := hashObject(commitObj)
		newObjs = append(newObjs, commitObj)

		packBytes, err := encodePack(dedupObjects(newObjs))
		if err != nil {
			return errs.PlainforgeBadReply
		}

		err = c.pushReceivePack(ctx, repo, ref, tipOID, newCommitOID, packBytes)
		if err == nil {
			c.invalidateCache(repo)
			return nil
		}
		if errors.Is(err, errPushRejected) {
			continue
		}
		return err
	}
	return errs.PlainforgeConflict
}

func (c *Client) invalidateCache(repo string) {
	c.mu.Lock()
	delete(c.cache, repo)
	c.mu.Unlock()
}

func cleanCommitMessage(app, summary string) (string, error) {
	if strings.ContainsRune(summary, 0) || strings.ContainsRune(summary, '\r') {
		return "", errs.Config
	}
	msg := strings.TrimSpace(summary)
	if msg == "" {
		msg = "shipkeeper: update " + app
	}
	if !strings.Contains(msg, skipCIMarker) {
		msg += " " + skipCIMarker
	}
	return msg + "\n", nil
}

func containsToken(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func buildUpdatedTreeObjects(objs map[string]gitObject, commitData []byte, app string, manifestBytes []byte) (string, []gitObject, error) {
	rootTreeOID := parseHeaderValue(commitData, "tree ")
	rootObj, ok := objs[rootTreeOID]
	if !ok || rootObj.kind != kindTree {
		return "", nil, errs.PlainforgeBadReply
	}
	rootEntries, err := parseTreeEntries(rootObj.data)
	if err != nil {
		return "", nil, errs.PlainforgeBadReply
	}

	blobObj := gitObject{kind: kindBlob, data: manifestBytes}
	blobOID := hashObject(blobObj)
	created := []gitObject{blobObj}

	// Prefer apps/<app>/app.yaml when present in a multi-app repository.
	if appsIdx := findTreeEntry(rootEntries, "apps"); appsIdx >= 0 && rootEntries[appsIdx].mode == "40000" {
		appsObj, ok := objs[rootEntries[appsIdx].oid]
		if ok && appsObj.kind == kindTree {
			appsEntries, err := parseTreeEntries(appsObj.data)
			if err != nil {
				return "", nil, errs.PlainforgeBadReply
			}
			if slugIdx := findTreeEntry(appsEntries, app); slugIdx >= 0 && appsEntries[slugIdx].mode == "40000" {
				slugObj, ok := objs[appsEntries[slugIdx].oid]
				if ok && slugObj.kind == kindTree {
					slugEntries, err := parseTreeEntries(slugObj.data)
					if err != nil {
						return "", nil, errs.PlainforgeBadReply
					}
					if fileIdx := findTreeEntry(slugEntries, "app.yaml"); fileIdx >= 0 {
						slugEntries[fileIdx] = treeEntry{mode: "100644", name: "app.yaml", oid: blobOID}
						slugTreeBytes, err := encodeTreeEntries(slugEntries)
						if err != nil {
							return "", nil, errs.PlainforgeBadReply
						}
						slugTreeObj := gitObject{kind: kindTree, data: slugTreeBytes}
						slugTreeOID := hashObject(slugTreeObj)
						created = append(created, slugTreeObj)

						appsEntries[slugIdx].oid = slugTreeOID
						appsTreeBytes, err := encodeTreeEntries(appsEntries)
						if err != nil {
							return "", nil, errs.PlainforgeBadReply
						}
						appsTreeObj := gitObject{kind: kindTree, data: appsTreeBytes}
						appsTreeOID := hashObject(appsTreeObj)
						created = append(created, appsTreeObj)

						rootEntries[appsIdx].oid = appsTreeOID
						rootTreeBytes, err := encodeTreeEntries(rootEntries)
						if err != nil {
							return "", nil, errs.PlainforgeBadReply
						}
						newRootObj := gitObject{kind: kindTree, data: rootTreeBytes}
						created = append(created, newRootObj)
						return hashObject(newRootObj), created, nil
					}
				}
			}
		}
	}

	if fileIdx := findTreeEntry(rootEntries, "app.yaml"); fileIdx >= 0 {
		rootEntries[fileIdx] = treeEntry{mode: "100644", name: "app.yaml", oid: blobOID}
		rootTreeBytes, err := encodeTreeEntries(rootEntries)
		if err != nil {
			return "", nil, errs.PlainforgeBadReply
		}
		newRootObj := gitObject{kind: kindTree, data: rootTreeBytes}
		created = append(created, newRootObj)
		return hashObject(newRootObj), created, nil
	}

	return "", nil, errs.PlainforgeNoApp
}

func parseTreeEntries(data []byte) ([]treeEntry, error) {
	var out []treeEntry
	rest := data
	for len(rest) > 0 {
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || nul+1+sha1.Size > len(rest) {
			return nil, errors.New("corrupt tree entry")
		}
		header := string(rest[:nul])
		mode, name, ok := strings.Cut(header, " ")
		if !ok || mode == "" || name == "" {
			return nil, errors.New("corrupt tree header")
		}
		oid := hex.EncodeToString(rest[nul+1 : nul+1+sha1.Size])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		rest = rest[nul+1+sha1.Size:]
	}
	return out, nil
}

func findTreeEntry(entries []treeEntry, name string) int {
	for i, e := range entries {
		if e.name == name {
			return i
		}
	}
	return -1
}

func treeSortKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

func encodeTreeEntries(entries []treeEntry) ([]byte, error) {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		rawOID, err := hex.DecodeString(e.oid)
		if err != nil || len(rawOID) != sha1.Size {
			return nil, errors.New("invalid tree entry oid")
		}
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(rawOID)
	}
	return buf.Bytes(), nil
}

func parseCommitterTimestamp(commitData []byte) int64 {
	for _, line := range strings.Split(string(commitData), "\n") {
		if line == "" {
			break
		}
		rest, ok := strings.CutPrefix(line, "committer ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return 0
		}
		ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64)
		if err != nil {
			return 0
		}
		return ts
	}
	return 0
}

func dedupObjects(objs []gitObject) []gitObject {
	seen := make(map[string]bool, len(objs))
	out := make([]gitObject, 0, len(objs))
	for _, o := range objs {
		oid := hashObject(o)
		if seen[oid] {
			continue
		}
		seen[oid] = true
		out = append(out, o)
	}
	return out
}

func encodePack(objs []gitObject) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])

	for _, obj := range objs {
		size := uint64(len(obj.data))
		first := byte((obj.kind&0x07)<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}

		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(obj.data); err != nil {
			_ = zw.Close()
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}

	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

func (c *Client) pushReceivePack(ctx context.Context, repo, ref, oldOID, newOID string, packBytes []byte) error {
	var body bytes.Buffer
	writePktString(&body, fmt.Sprintf("%s %s %s\x00report-status\n", oldOID, newOID, ref))
	body.WriteString("0000")
	body.Write(packBytes)

	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + repo + ".git/git-receive-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return errs.Config
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Accept", "application/x-git-receive-pack-result")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil {
		return transportError(err)
	}
	if len(raw) > maxRefsBytes {
		return errs.PlainforgeBadReply
	}
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
		return errPushRejected
	}
	if err := checkStatus(resp.StatusCode); err != nil {
		return err
	}

	return parseReceiveStatus(raw, ref)
}

func parseReceiveStatus(raw []byte, targetRef string) error {
	unwrapped, err := unwrapReceiveSideband(raw)
	if err != nil {
		return errs.PlainforgeBadReply
	}
	r := bytes.NewReader(unwrapped)
	unpackOK := false
	refOK := false
	for {
		pkt, flush, eof, err := readPktLine(r)
		if err != nil {
			return errs.PlainforgeBadReply
		}
		if eof || flush {
			break
		}
		line := strings.TrimSuffix(string(pkt), "\n")
		switch {
		case line == "unpack ok":
			unpackOK = true
		case strings.HasPrefix(line, "unpack "):
			return errs.PlainforgeBadReply
		case line == "ok "+targetRef:
			refOK = true
		case strings.HasPrefix(line, "ng "+targetRef+" ") || line == "ng "+targetRef:
			return errPushRejected
		}
	}
	if !unpackOK || !refOK {
		return errs.PlainforgeBadReply
	}
	return nil
}

func unwrapReceiveSideband(raw []byte) ([]byte, error) {
	r := bytes.NewReader(raw)
	var channel1 bytes.Buffer
	usedSideband := false
	for r.Len() > 0 {
		pkt, flush, eof, err := readPktLine(r)
		if err != nil {
			return nil, err
		}
		if eof || flush {
			break
		}
		if len(pkt) == 0 {
			continue
		}
		switch pkt[0] {
		case 1:
			usedSideband = true
			channel1.Write(pkt[1:])
		case 2:
			usedSideband = true
		case 3:
			return nil, errors.New("receive-pack sideband error")
		default:
			if usedSideband {
				return nil, errors.New("mixed sideband framing")
			}
			return raw, nil
		}
	}
	if usedSideband {
		return channel1.Bytes(), nil
	}
	return raw, nil
}
