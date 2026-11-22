package onchfs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tezos side of onchfs: the contract fxhash deployed on Tezos mainnet, read
// through on-chain views (a node's run_script_view RPC). Two views are used:
//
//	get_inode_at(Pair cid path) -> Pair cid (or (map string bytes)          ; Left: directory
//	                                            (Pair (list bytes) bytes))  ; Right: file chunks, metadata
//	read_file(cid)              -> Pair content metadata
//
// A directory address is derived exactly as on EVM. A file address is not:
// the Tezos contract commits to the content, keccak(0x01 ‖ keccak(content) ‖
// keccak(metadata)), where the EVM contracts commit to the chunk list (every
// file of two recorded mainnet trees checks out this way and none the EVM way;
// testdata/tezos-*.json). So the content read_file returns proves itself and
// no chunk is read. The formula is chosen by chain, never by trying both: on
// EVM the content form can be met with forged bytes (see Reader.verifyFile).
//
// Inode answers are decoded from Micheline JSON in the node's Optimized
// unparsing mode (a map is a list of Elt).

// TezosMainnet is the CAIP-2 id of Tezos mainnet.
const TezosMainnet = "tezos:NetXdQprcVkpaWU"

// TezosContract is the onchfs file system contract fxhash deployed on Tezos
// mainnet. It must match contentpath.OnchfsChains.
const TezosContract = "KT1Ae7dT1gsLw2tRnUMXSCmEyF74KVkM6LUo"

// TezosViewer runs one view of TezosContract at a pinned block and returns
// its result as Micheline JSON. A view that fails with
// ONCHFS_INODE_NOT_FOUND is ErrNotFound; any other failure is the node's.
type TezosViewer interface {
	View(ctx context.Context, block, view string, input json.RawMessage) (json.RawMessage, error)
}

// TezosReader reads and verifies onchfs trees from Tezos mainnet at one block.
type TezosReader struct {
	viewer TezosViewer
	block  string
	limits Limits
}

// NewTezosReader binds a viewer to block (a level or block hash), with
// DefaultLimits.
func NewTezosReader(viewer TezosViewer, block string) *TezosReader {
	return &TezosReader{viewer: viewer, block: block, limits: DefaultLimits}
}

// ReadTree reads the whole tree under root (64 hex) and verifies every inode.
func (r *TezosReader) ReadTree(ctx context.Context, root string) (*Tree, error) {
	files, err := readTree(ctx, r, root, r.limits)
	if err != nil {
		return nil, err
	}
	return &Tree{Root: root, Chain: TezosMainnet, Block: r.block, Files: files}, nil
}

// michelson is one Micheline node. Only the shapes these two views return are
// decoded; anything else fails the decode, which is ErrVerification.
type michelson struct {
	Prim   string            `json:"prim"`
	Args   []json.RawMessage `json:"args"`
	Bytes  *string           `json:"bytes"`
	String *string           `json:"string"`
}

func (r *TezosReader) readInode(ctx context.Context, cid string) (inode, error) {
	input := fmt.Sprintf(`{"prim":"Pair","args":[{"bytes":%q},[]]}`, cid)
	out, err := r.viewer.View(ctx, r.block, "get_inode_at", json.RawMessage(input))
	if err != nil {
		return inode{}, err
	}
	pair, err := decodePair(out)
	if err != nil {
		return inode{}, fmt.Errorf("%w: inode %s is malformed: %w", ErrVerification, cid, err)
	}
	got, err := decodeBytes(pair[0])
	if err != nil || hexString(got) != cid {
		return inode{}, fmt.Errorf("%w: asked for inode %s, got another", ErrVerification, cid)
	}
	var or michelson
	if err := json.Unmarshal(pair[1], &or); err != nil || len(or.Args) != 1 {
		return inode{}, fmt.Errorf("%w: inode %s is malformed", ErrVerification, cid)
	}
	switch or.Prim {
	case "Left":
		entries, err := decodeDirectory(or.Args[0])
		if err != nil {
			return inode{}, fmt.Errorf("%w: directory %s is malformed: %w", ErrVerification, cid, err)
		}
		if err := verifyDirectory(cid, entries); err != nil {
			return inode{}, err
		}
		return inode{isDir: true, entries: entries}, nil
	case "Right":
		file, err := decodePair(or.Args[0])
		if err != nil {
			return inode{}, fmt.Errorf("%w: file %s is malformed: %w", ErrVerification, cid, err)
		}
		var chunks []json.RawMessage
		if err := json.Unmarshal(file[0], &chunks); err != nil {
			return inode{}, fmt.Errorf("%w: file %s chunk list is malformed", ErrVerification, cid)
		}
		return r.readFile(ctx, cid, len(chunks))
	}
	return inode{}, fmt.Errorf("%w: inode %s is neither a directory nor a file", ErrVerification, cid)
}

// readFile reads a file's content and metadata in one view and proves them
// against the address.
func (r *TezosReader) readFile(ctx context.Context, cid string, chunks int) (inode, error) {
	out, err := r.viewer.View(ctx, r.block, "read_file", json.RawMessage(fmt.Sprintf(`{"bytes":%q}`, cid)))
	if err != nil {
		return inode{}, err
	}
	pair, err := decodePair(out)
	if err != nil {
		return inode{}, fmt.Errorf("%w: file %s is malformed: %w", ErrVerification, cid, err)
	}
	content, err := decodeBytes(pair[0])
	if err != nil {
		return inode{}, fmt.Errorf("%w: file %s content is malformed", ErrVerification, cid)
	}
	metadata, err := decodeBytes(pair[1])
	if err != nil {
		return inode{}, fmt.Errorf("%w: file %s metadata is malformed", ErrVerification, cid)
	}
	if hexString(keccak(append(append([]byte{1}, keccak(content)...), keccak(metadata)...))) != cid {
		return inode{}, fmt.Errorf("%w: file %s does not match its content and headers", ErrVerification, cid)
	}
	return inode{stored: content, metadata: metadata, chunks: chunks}, nil
}

func decodePair(raw json.RawMessage) ([]json.RawMessage, error) {
	var m michelson
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Prim != "Pair" || len(m.Args) != 2 {
		return nil, fmt.Errorf("expected a Pair, got %q with %d args", m.Prim, len(m.Args))
	}
	return m.Args, nil
}

func decodeBytes(raw json.RawMessage) ([]byte, error) {
	var m michelson
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m.Bytes == nil || len(*m.Bytes)%2 != 0 || !isHexString(*m.Bytes) {
		return nil, fmt.Errorf("expected bytes")
	}
	return mustHex(strings.ToLower(*m.Bytes)), nil
}

// decodeDirectory decodes a `map string bytes` (a list of Elt name checksum).
func decodeDirectory(raw json.RawMessage) ([]dirEntry, error) {
	var elts []michelson
	if err := json.Unmarshal(raw, &elts); err != nil {
		return nil, err
	}
	entries := make([]dirEntry, 0, len(elts))
	for _, e := range elts {
		if e.Prim != "Elt" || len(e.Args) != 2 {
			return nil, fmt.Errorf("expected Elt")
		}
		var name michelson
		if err := json.Unmarshal(e.Args[0], &name); err != nil || name.String == nil {
			return nil, fmt.Errorf("expected a name")
		}
		sum, err := decodeBytes(e.Args[1])
		if err != nil || len(sum) != 32 {
			return nil, fmt.Errorf("expected a 32-byte checksum for %q", *name.String)
		}
		entries = append(entries, dirEntry{name: *name.String, checksum: hexString(sum)})
	}
	return entries, nil
}
