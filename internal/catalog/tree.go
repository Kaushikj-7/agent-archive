package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/wangjohn/agent-archive/internal/storage"
)

const maxNodeBytes = 128 << 10

const fanout = 32

const maxDepth = 64

type item struct {
	Key   string          `json:"Key"`
	Value json.RawMessage `json:"Value"`
}

type branch struct {
	Max   string    `json:"Max"`
	Ref   ObjectRef `json:"Ref"`
	Count uint64    `json:"Count"`
}

type node struct {
	Count    uint64   `json:"Count"`
	Leaves   []item   `json:"leaves,omitempty"`
	Children []branch `json:"children,omitempty"`
}

func (w *Writer) readNode(ctx context.Context, ref ObjectRef) (node, error) {
	var n node
	if err := ref.validate(); err != nil {
		return n, err
	}
	if ref.Key == "" {
		return n, nil
	}
	raw, err := w.readRef(ctx, ref, maxNodeBytes)
	if err != nil {
		return n, err
	}
	return decodeNode(raw)
}

func decodeNode(raw []byte) (node, error) {
	var n node
	if err := json.Unmarshal(raw, &n); err != nil {
		return n, err
	}
	if len(n.Leaves)+len(n.Children) == 0 || len(n.Leaves) > fanout || len(n.Children) > fanout || len(n.Leaves) > 0 && len(n.Children) > 0 {
		return n, errors.New("invalid catalog node")
	}
	for i, v := range n.Leaves {
		if len(v.Key) > 4096 || i > 0 && n.Leaves[i-1].Key >= v.Key {
			return n, errors.New("invalid leaf order")
		}
	}
	var total uint64
	for i, v := range n.Children {
		if v.Ref.Key == "" || i > 0 && n.Children[i-1].Max >= v.Max {
			return n, errors.New("invalid branch order")
		}
		if v.Count > ^uint64(0)-total {
			return n, errors.New("catalog count overflow")
		}
		total += v.Count
	}
	if n.Count != nodeCount(n) {
		return n, errors.New("invalid node count")
	}
	return n, nil
}

func nodeCount(n node) uint64 {
	if len(n.Children) == 0 {
		return uint64(len(n.Leaves))
	}
	var total uint64
	for _, c := range n.Children {
		total += c.Count
	}
	return total
}

func (w *Writer) lookup(ctx context.Context, ref ObjectRef, key string) (json.RawMessage, error) {
	for range maxDepth {
		n, err := w.readNode(ctx, ref)
		if err != nil {
			return nil, err
		}
		if len(n.Children) == 0 {
			i := sort.Search(len(n.Leaves), func(i int) bool { return n.Leaves[i].Key >= key })
			if i < len(n.Leaves) && n.Leaves[i].Key == key {
				return n.Leaves[i].Value, nil
			}
			return nil, nil
		}
		i := sort.Search(len(n.Children), func(i int) bool { return n.Children[i].Max >= key })
		if i == len(n.Children) {
			return nil, nil
		}
		ref = n.Children[i].Ref
	}
	return nil, errors.New("catalog tree depth exceeded")
}

// change writes only the root-to-leaf path. Deletion removes empty paths;
// splitting maintains bounded fanout, without an archive-wide rebuild.
func (w *Writer) change(ctx context.Context, ref ObjectRef, key string, value json.RawMessage, depth int) ([]branch, error) {
	if depth >= maxDepth {
		return nil, errors.New("catalog tree depth exceeded")
	}
	n, err := w.readNode(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(n.Children) == 0 {
		i := sort.Search(len(n.Leaves), func(i int) bool { return n.Leaves[i].Key >= key })
		if i < len(n.Leaves) && n.Leaves[i].Key == key {
			n.Leaves = append(n.Leaves[:i], n.Leaves[i+1:]...)
		}
		if value != nil {
			n.Leaves = append(n.Leaves, item{})
			copy(n.Leaves[i+1:], n.Leaves[i:])
			n.Leaves[i] = item{Key: key, Value: value}
		}
	} else {
		i := sort.Search(len(n.Children), func(i int) bool { return n.Children[i].Max >= key })
		if i == len(n.Children) {
			i--
		}
		changed, e := w.change(ctx, n.Children[i].Ref, key, value, depth+1)
		if e != nil {
			return nil, e
		}
		children := append([]branch{}, n.Children[:i]...)
		children = append(children, changed...)
		children = append(children, n.Children[i+1:]...)
		n.Children = children
	}
	count := len(n.Leaves) + len(n.Children)
	if count == 0 {
		return nil, nil
	}
	parts, err := splitNode(n)
	if err != nil {
		return nil, err
	}
	var out []branch
	for _, p := range parts {
		p.Count = nodeCount(p)
		ref, e := w.putJSON(ctx, KindNodes, p, maxNodeBytes)
		if e != nil {
			return nil, e
		}
		var lastKey string
		if len(p.Children) > 0 {
			lastKey = p.Children[len(p.Children)-1].Max
		} else {
			lastKey = p.Leaves[len(p.Leaves)-1].Key
		}
		out = append(out, branch{Max: lastKey, Ref: ref, Count: p.Count})
	}
	return out, nil
}

func (w *Writer) update(ctx context.Context, ref ObjectRef, key string, value any) (ObjectRef, error) {
	if len(key) > 4096 {
		return ObjectRef{}, errors.New("catalog index key exceeds bound")
	}
	var raw json.RawMessage
	if value != nil {
		b, err := encodeTreeValue(key, value)
		if err != nil {
			return ObjectRef{}, err
		}
		raw = b
	}
	parts, err := w.change(ctx, ref, key, raw, 0)
	if err != nil {
		return ObjectRef{}, err
	}
	if len(parts) == 0 {
		return ObjectRef{}, nil
	}
	if len(parts) == 1 {
		return parts[0].Ref, nil
	}
	n := node{Children: parts}
	n.Count = nodeCount(n)
	return w.putJSON(ctx, KindNodes, n, maxNodeBytes)
}

func (w *Writer) readRef(ctx context.Context, ref ObjectRef, limit int64) ([]byte, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	if ref.Key == "" {
		return nil, errors.New("catalog object reference required")
	}
	raw, err := w.bounded.GetLimited(ctx, ref.Key, limit)
	if err != nil {
		return nil, err
	}
	if len(raw) > int(limit) {
		return nil, storage.ErrObjectTooLarge
	}
	if !storage.VerifySHA256(raw, ref.SHA256) {
		return nil, storage.ErrChecksumMismatch
	}
	return raw, nil
}

func splitNode(n node) ([]node, error) {
	raw, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	count := len(n.Leaves) + len(n.Children)
	if count <= fanout && len(raw) <= maxNodeBytes {
		return []node{n}, nil
	}
	if count <= 1 {
		return nil, storage.ErrObjectTooLarge
	}
	mid := count / 2
	left, right := splitHalves(n, mid)
	a, err := splitNode(left)
	if err != nil {
		return nil, err
	}
	b, err := splitNode(right)
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

func splitHalves(n node, mid int) (node, node) {
	if len(n.Children) > 0 {
		return node{Children: n.Children[:mid]}, node{Children: n.Children[mid:]}
	}
	return node{Leaves: n.Leaves[:mid]}, node{Leaves: n.Leaves[mid:]}
}
