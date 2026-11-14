package plainforge

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ClearedStack/shipkeeper/internal/errs"
)

type objKind uint8

const (
	kindCommit   objKind = 1
	kindTree     objKind = 2
	kindBlob     objKind = 3
	kindTag      objKind = 4
	kindOFSDelta objKind = 6
	kindRefDelta objKind = 7
)

type gitObject struct {
	kind objKind
	data []byte
}

type rawEntry struct {
	offset     int
	kind       objKind
	baseOffset int
	baseOID    string
	data       []byte
}

func unpack(pack []byte) (map[string]gitObject, error) {
	if len(pack) < 12+sha1.Size || string(pack[:4]) != "PACK" {
		return nil, errors.New("bad pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 {
		return nil, errors.New("unsupported pack version")
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	if count > maxObjects {
		return nil, errors.New("too many objects in pack")
	}

	body := pack[:len(pack)-sha1.Size]
	wantSum := pack[len(pack)-sha1.Size:]
	gotSum := sha1.Sum(body)
	if !bytes.Equal(gotSum[:], wantSum) {
		return nil, errors.New("pack checksum mismatch")
	}

	r := bytes.NewReader(body[12:])
	entries := make([]rawEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		entryPos := 12 + (len(body) - 12 - r.Len())
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		kind := objKind((b >> 4) & 0x07)
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if shift > 24 {
				return nil, errors.New("object size varint too large")
			}
			b, err = r.ReadByte()
			if err != nil {
				return nil, err
			}
			size |= uint64(b&0x7f) << shift
			shift += 7
		}
		if size > maxObjectSize {
			return nil, errors.New("object exceeds size limit")
		}

		ent := rawEntry{offset: entryPos, kind: kind}
		switch kind {
		case kindCommit, kindTree, kindBlob, kindTag:
		case kindOFSDelta:
			neg, err := readOffsetDelta(r)
			if err != nil || neg <= 0 || neg > entryPos {
				return nil, errors.New("invalid ofs-delta offset")
			}
			ent.baseOffset = entryPos - neg
		case kindRefDelta:
			var rawOID [20]byte
			if _, err := io.ReadFull(r, rawOID[:]); err != nil {
				return nil, err
			}
			ent.baseOID = hex.EncodeToString(rawOID[:])
		default:
			return nil, errors.New("unknown pack object kind")
		}

		inflated, err := inflateOne(r, int(size))
		if err != nil {
			return nil, err
		}
		ent.data = inflated
		entries = append(entries, ent)
	}
	if r.Len() != 0 {
		return nil, errors.New("trailing bytes in pack")
	}

	byOffset := make(map[int]gitObject, len(entries))
	byOID := make(map[string]gitObject, len(entries))

	// Resolve whole objects and offset deltas in pack order, then resolve any
	// remaining ref-deltas in bounded passes.
	unresolved := make([]rawEntry, 0)
	for _, ent := range entries {
		switch ent.kind {
		case kindCommit, kindTree, kindBlob, kindTag:
			obj := gitObject{kind: ent.kind, data: ent.data}
			byOffset[ent.offset] = obj
			byOID[hashObject(obj)] = obj
		case kindOFSDelta:
			base, ok := byOffset[ent.baseOffset]
			if !ok {
				unresolved = append(unresolved, ent)
				continue
			}
			merged, err := applyDelta(base.data, ent.data)
			if err != nil {
				return nil, err
			}
			obj := gitObject{kind: base.kind, data: merged}
			byOffset[ent.offset] = obj
			byOID[hashObject(obj)] = obj
		case kindRefDelta:
			base, ok := byOID[ent.baseOID]
			if !ok {
				unresolved = append(unresolved, ent)
				continue
			}
			merged, err := applyDelta(base.data, ent.data)
			if err != nil {
				return nil, err
			}
			obj := gitObject{kind: base.kind, data: merged}
			byOffset[ent.offset] = obj
			byOID[hashObject(obj)] = obj
		}
	}

	for len(unresolved) > 0 {
		progressed := false
		next := unresolved[:0]
		for _, ent := range unresolved {
			var base gitObject
			var ok bool
			if ent.kind == kindOFSDelta {
				base, ok = byOffset[ent.baseOffset]
			} else {
				base, ok = byOID[ent.baseOID]
			}
			if !ok {
				next = append(next, ent)
				continue
			}
			merged, err := applyDelta(base.data, ent.data)
			if err != nil {
				return nil, err
			}
			obj := gitObject{kind: base.kind, data: merged}
			byOffset[ent.offset] = obj
			byOID[hashObject(obj)] = obj
			progressed = true
		}
		if !progressed {
			return nil, errors.New("unresolved delta in pack")
		}
		unresolved = next
	}

	return byOID, nil
}

func readOffsetDelta(r *bytes.Reader) (int, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	val := uint64(b & 0x7f)
	for i := 0; b&0x80 != 0; i++ {
		if i > 8 {
			return 0, errors.New("offset delta overflow")
		}
		b, err = r.ReadByte()
		if err != nil {
			return 0, err
		}
		val = ((val + 1) << 7) | uint64(b&0x7f)
	}
	return int(val), nil
}

type byteReadReader struct {
	*bytes.Reader
}

func inflateOne(r *bytes.Reader, wantSize int) ([]byte, error) {
	zr, err := zlib.NewReader(byteReadReader{r})
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(wantSize)+1))
	if err != nil {
		return nil, err
	}
	if len(out) != wantSize {
		return nil, errors.New("inflated size mismatch")
	}
	return out, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	r := bytes.NewReader(delta)
	srcSize, err := readSizeLEB(r)
	if err != nil || srcSize != uint64(len(base)) {
		return nil, errors.New("delta base size mismatch")
	}
	dstSize, err := readSizeLEB(r)
	if err != nil || dstSize > maxObjectSize {
		return nil, errors.New("delta target size out of bounds")
	}
	out := make([]byte, 0, dstSize)
	for r.Len() > 0 {
		op, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if op&0x80 != 0 {
			var cpOff, cpSize uint32
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, err
					}
					cpOff |= uint32(b) << (8 * i)
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					b, err := r.ReadByte()
					if err != nil {
						return nil, err
					}
					cpSize |= uint32(b) << (8 * i)
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			end := uint64(cpOff) + uint64(cpSize)
			if end > uint64(len(base)) || uint64(len(out))+uint64(cpSize) > dstSize {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[cpOff:end]...)
		} else if op > 0 {
			if int(op) > r.Len() || uint64(len(out))+uint64(op) > dstSize {
				return nil, errors.New("delta insert out of range")
			}
			chunk := make([]byte, int(op))
			_, _ = io.ReadFull(r, chunk)
			out = append(out, chunk...)
		} else {
			return nil, errors.New("reserved delta opcode zero")
		}
	}
	if uint64(len(out)) != dstSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out, nil
}

func readSizeLEB(r *bytes.Reader) (uint64, error) {
	var val uint64
	var shift uint
	for {
		if shift > 28 {
			return 0, errors.New("size varint overflow")
		}
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		val |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return val, nil
		}
		shift += 7
	}
}

func hashObject(obj gitObject) string {
	var name string
	switch obj.kind {
	case kindCommit:
		name = "commit"
	case kindTree:
		name = "tree"
	case kindBlob:
		name = "blob"
	case kindTag:
		name = "tag"
	}
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", name, len(obj.data))
	h.Write(obj.data)
	return hex.EncodeToString(h.Sum(nil))
}

func extractAppFiles(objs map[string]gitObject, commitOID string) (map[string][]byte, error) {
	commit, ok := objs[commitOID]
	for depth := 0; ok && commit.kind == kindTag && depth < 8; depth++ {
		target := parseHeaderValue(commit.data, "object ")
		commit, ok = objs[target]
	}
	if !ok || commit.kind != kindCommit {
		return nil, errs.PlainforgeBadReply
	}
	treeOID := parseHeaderValue(commit.data, "tree ")
	root, ok := objs[treeOID]
	if !ok || root.kind != kindTree {
		return nil, errs.PlainforgeBadReply
	}

	entries, err := parseTree(root.data)
	if err != nil {
		return nil, errs.PlainforgeBadReply
	}
	files := map[string][]byte{}
	if oid, ok := entries["app.yaml"]; ok {
		blob, exists := objs[oid]
		if !exists || blob.kind != kindBlob || len(blob.data) > maxAppBytes {
			return nil, errs.PlainforgeBadReply
		}
		files["app.yaml"] = blob.data
	}
	if appsOID, ok := entries["apps"]; ok {
		if appsTree, exists := objs[appsOID]; exists && appsTree.kind == kindTree {
			subDirs, err := parseTree(appsTree.data)
			if err != nil {
				return nil, errs.PlainforgeBadReply
			}
			for slug, dirOID := range subDirs {
				dirObj, exists := objs[dirOID]
				if !exists || dirObj.kind != kindTree {
					continue
				}
				dirEntries, err := parseTree(dirObj.data)
				if err != nil {
					return nil, errs.PlainforgeBadReply
				}
				if appOID, has := dirEntries["app.yaml"]; has {
					blob, exists := objs[appOID]
					if !exists || blob.kind != kindBlob || len(blob.data) > maxAppBytes {
						return nil, errs.PlainforgeBadReply
					}
					files["apps/"+slug+"/app.yaml"] = blob.data
				}
			}
		}
	}
	return files, nil
}

func parseHeaderValue(data []byte, prefix string) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if val, ok := strings.CutPrefix(line, prefix); ok && oidRE.MatchString(val) {
			return val
		}
	}
	return ""
}

func parseTree(data []byte) (map[string]string, error) {
	out := map[string]string{}
	rest := data
	for len(rest) > 0 {
		nul := bytes.IndexByte(rest, 0)
		if nul < 0 || nul+1+sha1.Size > len(rest) {
			return nil, errors.New("corrupt tree entry")
		}
		header := string(rest[:nul])
		_, name, ok := strings.Cut(header, " ")
		if !ok || name == "" {
			return nil, errors.New("corrupt tree header")
		}
		oid := hex.EncodeToString(rest[nul+1 : nul+1+sha1.Size])
		out[name] = oid
		rest = rest[nul+1+sha1.Size:]
	}
	return out, nil
}
