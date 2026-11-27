package uia

import (
	"context"
	"errors"
	"fmt"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// ErrTreeTruncated is returned when a desktop accessibility tree exceeds the configured max_nodes bound.
var ErrTreeTruncated = errors.New("uia accessibility tree exceeded max_nodes cap")

type uiaWalkOps[E comparable] struct {
	firstChild  func(E) (E, error)
	nextSibling func(E) (E, error)
	makeNode    func(el E, parentID string) contract.AXNode
	release     func(E)
}

type uiaQueueItem[E comparable] struct {
	el      E
	nodeIdx int
	depth   int
	ownsEl  bool
}

// walkUIASubtree performs a bounded level-order BFS walk over an accessibility subtree,
// releasing every owned element handle and failing closed on context cancellation or maxNodes truncation.
func walkUIASubtree[E comparable](
	ctx context.Context,
	start E,
	maxNodes int,
	maxDepth int,
	ops uiaWalkOps[E],
) ([]contract.AXNode, error) {
	var zero E
	if start == zero {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxNodes <= 0 {
		return nil, fmt.Errorf("%w: limit %d", ErrTreeTruncated, maxNodes)
	}

	out := make([]contract.AXNode, 0, min(maxNodes, 256))
	out = append(out, ops.makeNode(start, ""))
	queue := []uiaQueueItem[E]{{el: start, nodeIdx: 0, depth: 0, ownsEl: false}}
	defer func() {
		for _, rem := range queue {
			if rem.ownsEl && rem.el != zero {
				ops.release(rem.el)
			}
		}
	}()

	releaseOwned := func(item uiaQueueItem[E]) {
		if item.ownsEl && item.el != zero {
			ops.release(item.el)
		}
	}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cur := queue[0]
		queue = queue[1:]

		if cur.depth < maxDepth {
			var childIDs []string
			parentID := out[cur.nodeIdx].NodeID
			ch, err := ops.firstChild(cur.el)
			if err == nil {
				for ch != zero {
					if cerr := ctx.Err(); cerr != nil {
						ops.release(ch)
						releaseOwned(cur)
						return nil, cerr
					}
					if len(out) >= maxNodes {
						ops.release(ch)
						releaseOwned(cur)
						return nil, fmt.Errorf("%w: limit %d reached", ErrTreeTruncated, maxNodes)
					}
					sib, _ := ops.nextSibling(ch)
					cNode := ops.makeNode(ch, parentID)
					cIdx := len(out)
					out = append(out, cNode)
					childIDs = append(childIDs, cNode.NodeID)
					if cur.depth+1 < maxDepth {
						queue = append(queue, uiaQueueItem[E]{el: ch, nodeIdx: cIdx, depth: cur.depth + 1, ownsEl: true})
					} else {
						ops.release(ch)
					}
					ch = sib
				}
			}
			out[cur.nodeIdx].ChildIDs = childIDs
		}

		releaseOwned(cur)
	}
	return out, nil
}
