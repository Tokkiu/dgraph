/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package query

import (
	"context"
	"errors"
	"strings"
	"time"

	otrace "go.opencensus.io/trace"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/types/facets"
	"github.com/dgraph-io/dgraph/worker"
	"github.com/dgraph-io/dgraph/x"
	"github.com/golang/glog"
)

func ApplyMutations(ctx context.Context, m *pb.Mutations) (*api.TxnContext, error) {
	if worker.Config.ExpandEdge {
		edges, err := expandEdges(ctx, m)
		if err != nil {
			return nil, x.Wrapf(err, "While adding pb.edges")
		}
		m.Edges = edges
	} else {
		for _, mu := range m.Edges {
			if mu.Attr == x.Star && !worker.Config.ExpandEdge {
				return nil, x.Errorf("Expand edge (--expand_edge) is set to false." +
					" Cannot perform S * * deletion.")
			}
		}
	}
	tctx, err := worker.MutateOverNetwork(ctx, m)
	if err != nil {
		if span := otrace.FromContext(ctx); span != nil {
			span.Annotatef(nil, "MutateOverNetwork Error: %v. Mutation: %v.", err, m)
		}
	}
	return tctx, err
}

func expandEdges(ctx context.Context, m *pb.Mutations) ([]*pb.DirectedEdge, error) {
	edges := make([]*pb.DirectedEdge, 0, 2*len(m.Edges))
	for _, edge := range m.Edges {
		x.AssertTrue(edge.Op == pb.DirectedEdge_DEL || edge.Op == pb.DirectedEdge_SET)

		var preds []string
		if edge.Attr != x.Star {
			preds = []string{edge.Attr}
		} else {
			sg := &SubGraph{}
			sg.DestUIDs = &pb.List{Uids: []uint64{edge.GetEntity()}}
			sg.ReadTs = m.StartTs
			valMatrix, err := getNodePredicates(ctx, sg)
			if err != nil {
				return nil, err
			}
			if len(valMatrix) != 1 {
				return nil, x.Errorf("Expected only one list in value matrix while deleting: %v",
					edge.GetEntity())
			}
			for _, tv := range valMatrix[0].Values {
				if len(tv.Val) > 0 {
					preds = append(preds, string(tv.Val))
				}
			}
		}

		for _, pred := range preds {
			edgeCopy := *edge
			edgeCopy.Attr = pred
			edges = append(edges, &edgeCopy)

			// We only want to delete the pred from <uid> + <_predicate_> posting list if this is
			// a SP* deletion operation. Otherwise we just continue.
			if edge.Op == pb.DirectedEdge_DEL && string(edge.Value) != x.Star {
				continue
			}

			e := &pb.DirectedEdge{
				Op:     edge.Op,
				Entity: edge.GetEntity(),
				Attr:   "_predicate_",
				Value:  []byte(pred),
			}
			edges = append(edges, e)
		}
	}
	return edges, nil
}

func verifyUid(ctx context.Context, uid uint64) error {
	if uid <= worker.MaxLeaseId() {
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lease := worker.MaxLeaseId()
			if uid <= lease {
				return nil
			}
			if time.Now().After(deadline) {
				err := x.Errorf("Uid: [%d] cannot be greater than lease: [%d]", uid, lease)
				glog.V(2).Infof("verifyUid returned error: %v", err)
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// AssignUids tries to assign unique ids to each identity in the subjects and objects in the
// format of _:xxx. An identity, e.g. _:a, will only be assigned one uid regardless how many times
// it shows up in the subjects or objects
func AssignUids(ctx context.Context, nquads []*api.NQuad) (map[string]uint64, error) {
	newUids := make(map[string]uint64)
	num := &pb.Num{}
	var err error
	for _, nq := range nquads {
		// We dont want to assign uids to these.
		if nq.Subject == x.Star && nq.ObjectValue.GetDefaultVal() == x.Star {
			return newUids, errors.New("Predicate deletion should be called via alter")
		}

		if len(nq.Subject) == 0 {
			return nil, x.Errorf("Subject must not be empty for nquad: %+v", nq)
		}
		var uid uint64
		if strings.HasPrefix(nq.Subject, "_:") {
			newUids[nq.Subject] = 0
		} else if uid, err = gql.ParseUid(nq.Subject); err != nil {
			return newUids, err
		}
		if err = verifyUid(ctx, uid); err != nil {
			return newUids, err
		}

		if len(nq.ObjectId) > 0 {
			var uid uint64
			if strings.HasPrefix(nq.ObjectId, "_:") {
				newUids[nq.ObjectId] = 0
			} else if uid, err = gql.ParseUid(nq.ObjectId); err != nil {
				return newUids, err
			}
			if err = verifyUid(ctx, uid); err != nil {
				return newUids, err
			}
		}
	}

	num.Val = uint64(len(newUids))
	if int(num.Val) > 0 {
		var res *pb.AssignedIds
		// TODO: Optimize later by prefetching. Also consolidate all the UID requests into a single
		// pending request from this server to zero.
		if res, err = worker.AssignUidsOverNetwork(ctx, num); err != nil {
			return newUids, err
		}
		curId := res.StartId
		// assign generated ones now
		for k := range newUids {
			x.AssertTruef(curId != 0 && curId <= res.EndId, "not enough uids generated")
			newUids[k] = curId
			curId++
		}
	}
	return newUids, nil
}

func ToInternal(gmu *gql.Mutation,
	newUids map[string]uint64) (edges []*pb.DirectedEdge, err error) {

	// Wrapper for a pointer to protos.Nquad
	var wnq *gql.NQuad

	parse := func(nq *api.NQuad, op pb.DirectedEdge_Op) error {
		wnq = &gql.NQuad{NQuad: nq}
		if len(nq.Subject) == 0 {
			return nil
		}
		// Get edge from nquad using newUids.
		var edge *pb.DirectedEdge
		edge, err = wnq.ToEdgeUsing(newUids)
		if err != nil {
			return x.Wrap(err)
		}
		edge.Op = op
		edges = append(edges, edge)
		return nil
	}

	for _, nq := range gmu.Set {
		if err := facets.SortAndValidate(nq.Facets); err != nil {
			return edges, err
		}
		if err := parse(nq, pb.DirectedEdge_SET); err != nil {
			return edges, err
		}
	}
	for _, nq := range gmu.Del {
		if nq.Subject == x.Star && nq.ObjectValue.GetDefaultVal() == x.Star {
			return edges, errors.New("Predicate deletion should be called via alter")
		}
		if err := parse(nq, pb.DirectedEdge_DEL); err != nil {
			return edges, err
		}
	}

	return edges, nil
}
