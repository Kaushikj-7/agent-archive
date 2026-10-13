package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// Pressure is inserted at an actual captured Reserve call after the genuine
// proof loan is established. The same callback/assertions work on both arms.
func TestSettledIndexDescendantsPreserveCapturedPressureCause(t *testing.T) {
	for _, route := range []string{"frozen-head", "frozen-node", "second-head"} {
		t.Run(route, func(t *testing.T) {
			for _, pressure := range []bool{false, true} {
				t.Run(map[bool]string{false: "adequate", true: "pressure"}[pressure], func(t *testing.T) {
					_, captureReg, request, proof, budget := settledStateCaptureFixture(t)
					s := newTestStore(t)
					key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "owned-generation"}
					reg := migrationRegistration(key, "generation-owner")
					reg.PreviousGenerationID = "previous-owner"
					reg.CaptureFrozen = route != "second-head"
					head := generationHead{Version: 1, Key: key, Active: reg.ArchiveSessionID}
					if reg.CaptureFrozen {
						head.Active = "active-successor"
					}
					node := generationNode{Version: 1, Key: key, ID: reg.ArchiveSessionID, Previous: reg.PreviousGenerationID}
					receipt := generationRecovery{Version: 1, Key: key, Previous: reg.PreviousGenerationID, Next: reg.ArchiveSessionID, Complete: true}
					for _, dir := range []string{generationHeadsDir, generationNodesDir, generationRecoveryDir} {
						if err := os.MkdirAll(filepath.Join(s.home, dir), 0700); err != nil {
							t.Fatal(err)
						}
					}
					for _, write := range []struct {
						path  string
						value any
					}{
						{path: s.registrationPath(reg.ArchiveSessionID), value: reg},
						{path: s.generationHeadPath(key), value: head},
						{path: s.generationNodePath(reg.ArchiveSessionID), value: node},
						{path: s.generationRecoveryPath(reg.PreviousGenerationID), value: receipt},
					} {
						if err := local.Write(write.path, write.value); err != nil {
							t.Fatal(err)
						}
					}
					before := settledProtocolInventory(t, s.home)
					blocker := budget.Reservation()
					held := int64(0)
					boundCalls, scratchCalls := 0, 0
					reached := false
					err := proof.WithVerifiedCapture(t.Context(), captureReg, request, budget, func(ctx context.Context, _ archive.SourceBundle, _ nativefacts.SettledImportFacts) error {
						lease, err := request.Borrow()
						if err != nil {
							return err
						}
						defer lease.Close()
						scoped, closeScoped := s.WithReadBudget(ctx, budget)
						defer closeScoped()
						scoped.resourceReservation = func() stateReservation {
							return &causePressureReservation{stateReservation: lease.Reservation(), before: func(n int64) {
								if n == 2*4097+(32<<10) {
									boundCalls++
								}
								if n == 32<<10 {
									scratchCalls++
								}
								target := route == "frozen-head" && n == 2*4097+(32<<10) && boundCalls == 2 || route == "frozen-node" && n == 32<<10 && scratchCalls == 2 || route == "second-head" && n == 2*4097+(32<<10) && boundCalls == 3
								if !target || reached {
									return
								}
								reached = true
								if pressure {
									held = budget.Available() - (16 << 10)
									if held < 0 || !blocker.Reserve(held) {
										t.Fatal("late pressure setup")
									}
								}
							}}
						}
						if route == "second-head" {
							valid, err := scoped.matchingRegistration(key, reg.ArchiveSessionID)
							if err != nil {
								return err
							}
							if !valid {
								return errors.New("adequate generation owner missing")
							}
							return nil
						}
						valid, err := scoped.matchingCandidateParent(key, reg.ArchiveSessionID)
						if err != nil {
							return err
						}
						if !valid {
							return errors.New("adequate frozen parent missing")
						}
						return nil
					})
					if !reached {
						t.Fatal("targeted actual reservation boundary not reached")
					}
					if pressure {
						if !errors.Is(err, agentapi.ErrReadBudget) {
							t.Fatal("captured descendant resource cause lost", err)
						}
						classification := ErrSessionIndexRecoveryRequired
						if route == "frozen-node" {
							classification = ErrSessionIdentityConflict
						}
						if !errors.Is(err, classification) {
							t.Fatal("existing descendant classification lost", err)
						}
					} else if err != nil {
						t.Fatal("adequate real descendant refused", err)
					}
					assertSettledIndexInventory(t, before, settledProtocolInventory(t, s.home))
					blocker.Release(held)
					if err = proof.Close(); err != nil {
						t.Fatal(err)
					}
					request.Close()
					if budget.Available() != 128<<20 {
						t.Fatal("descendant owner leaked")
					}
				})
			}
		})
	}
}

type causePressureReservation struct {
	stateReservation
	before func(int64)
}

func (r *causePressureReservation) Reserve(n int64) bool {
	r.before(n)
	return r.stateReservation.Reserve(n)
}
