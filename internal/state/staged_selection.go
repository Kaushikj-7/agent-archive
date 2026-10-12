package state

import (
 "bytes"
 "context"
 "encoding/json"
 "errors"
 "time"
 "strings"

 "github.com/wangjohn/agent-archive/internal/agentmeta"
 "github.com/wangjohn/agent-archive/internal/archive"
 "github.com/wangjohn/agent-archive/internal/config"
)

// StagedSelection is a separately mandatory codec; ordinary selection2 never
// acquires staged meaning. Stage and request authority are derived by Store.
type StagedSelection struct {
 Version int `json:"version"`
 StageSHA256 string `json:"stage_sha256"`
 RosterSHA256 string `json:"roster_sha256"`
 CoveredToken string `json:"covered_token"`
 HadRequest bool `json:"had_request"`
 Core ReadySelection `json:"proof"`
 SHA256 string `json:"sha256"`
}

func (q *StagedSelection) UnmarshalJSON(raw []byte) error {
 if err := strictStagedKeys(raw, []string{"version","stage_sha256","roster_sha256","covered_token","had_request","proof","sha256"}); err != nil { return err }
 type plain StagedSelection
 var v plain
 d := json.NewDecoder(bytes.NewReader(raw)); d.DisallowUnknownFields()
 if err := d.Decode(&v); err != nil { return ErrDurableStorageRecovery }
 var fields map[string]json.RawMessage
 if json.Unmarshal(raw,&fields) != nil { return ErrDurableStorageRecovery }
 if err := rejectStagedNullAliases(fields["proof"],[]string{"version","bundle_sha256","metadata_sha256","source_set_sha256","binding_sha256","destination_id","admission_context","policy_context","predecessor","predecessor_sha256","purpose","sha256"}); err != nil { return err }
 *q = StagedSelection(v)
 return q.validateDigest()
}

func (q *StagedSelection) validateDigest() error {
 if q == nil || q.Version != 3 || q.Core.Version != 3 || q.Core.Purpose != "staged-import" || q.Core.SHA256 != "" || !readyDigestValid(q.StageSHA256) || !readyDigestValid(q.RosterSHA256) || !readyDigestValid(q.SHA256) || q.HadRequest != (q.CoveredToken != "") { return ErrDurableStorageRecovery }
 v := *q; v.SHA256 = ""
 raw, err := json.Marshal(v)
 if err != nil || readySHA(raw) != q.SHA256 { return ErrDurableStorageRecovery }
 return nil
}

func (p PendingPublication) validateStagedShape() error {
 if p.StagedSelection == nil { return nil }
 q := p.StagedSelection
 if p.JournalVersion != 3 || p.Selection != nil || p.Catalog != nil || p.MetadataOnly || p.History != nil || p.Bundle.History != nil || p.ImportExpectedMetadataSHA256 != "" || len(p.ImportRetired) != 0 || p.RequestToken != q.CoveredToken { return ErrReadySelectionFrozen }
 if err := q.validateDigest(); err != nil { return err }
 for _, h := range []string{q.Core.BundleSHA256,q.Core.MetadataSHA256,q.Core.SourceSetSHA256,q.Core.BindingSHA256,q.Core.AdmissionContext,q.Core.PolicyContext} { if !readyDigestValid(h) { return ErrDurableStorageRecovery } }
 switch q.Core.Predecessor {
 case "absent", "unknown": if q.Core.PredecessorSHA256 != "" { return ErrDurableStorageRecovery }
 case "present": if !readyDigestValid(q.Core.PredecessorSHA256) { return ErrDurableStorageRecovery }
 default: return ErrDurableStorageRecovery
 }
 return nil
}

func (s *Store) stagedContext(ctx context.Context, id string) (config.Config, archive.SessionRegistration, ImportStage, error) {
 cfg, err := s.inspectDurableReadRoots()
 if err != nil || !cfg.SupportsStagedSelection() { return cfg, archive.SessionRegistration{}, ImportStage{}, errors.Join(ErrReadySelectionFrozen,err) }
 reg, found, err := s.LoadRegistration(id)
 if err != nil || !found || reg.StagedImportSHA256 == "" || reg.CaptureFrozen || reg.HistoricalProject != nil || reg.Harness.Name == "cursor" || !cfg.AcceptSession(reg) || reg.DestinationID != cfg.DestinationID() { return cfg,reg,ImportStage{},errors.Join(ErrReadySelectionFrozen,err) }
 stage, found, err := s.LoadImportStage(ctx,reg)
 if err != nil || !found || stage.Version != 2 || !stage.TypedSelection || len(stage.Preserved) != 0 || stage.Bundle.History != nil || len(stage.Children) > 64 { return cfg,reg,stage,errors.Join(ErrReadySelectionFrozen,err) }
 if _, err := agentmeta.JSONWireBound(ctx,stage.Children,64<<10); err != nil { return cfg,reg,stage,errors.Join(ErrDurableStorageCapacity,err) }
 return cfg,reg,stage,nil
}

// FreezeStagedSelection accepts only freshly prepared typed noncatalog stages.
// It never promotes a legacy stage or obtains evidence from native sources.
func (s *Store) FreezeStagedSelection(ctx context.Context,id string,p PendingPublication,predecessor string,previous []byte) (PendingPublication,error) {
 if p.StagedSelection != nil || p.Selection != nil || p.JournalVersion != 0 || p.Catalog != nil { return p,ErrReadySelectionFrozen }
 cfg,reg,stage,err := s.stagedContext(ctx,id); if err != nil { return p,err }
 if !readySelectionOwnerMatches(reg,p.Bundle) || p.Bundle.History != nil || p.History != nil || p.MetadataOnly { return p,ErrReadySelectionFrozen }
 request,found,err := s.LoadRequest(id); if err != nil { return p,err }
 if found && request.Token != p.RequestToken || !found && p.RequestToken != "" { return p,ErrDurableStorageRecovery }
 q := &StagedSelection{Version:3,StageSHA256:stage.SHA256,CoveredToken:p.RequestToken,HadRequest:found,Core:ReadySelection{Version:3,Purpose:"staged-import",DestinationID:cfg.DestinationID(),Predecessor:predecessor,MetadataSHA256:readySHA(p.MetadataBytes)}}
 if predecessor == "present" { q.Core.PredecessorSHA256 = readySHA(previous) }
 q.RosterSHA256,err = s.readyDigest(ctx,stage.Children); if err != nil { return p,err }
 q.Core.AdmissionContext,err = s.readyDigest(ctx,reg); if err != nil { return p,err }
 q.Core.PolicyContext,err = s.readyDigest(ctx,struct{Filter,Adapter,Version,Skill string}{p.Bundle.Capture.FilterVersion,p.Bundle.Capture.AdapterName,p.Bundle.Capture.AdapterVersion,p.SkillEvidence}); if err != nil { return p,err }
 q.Core.BundleSHA256,err = s.readyDigest(ctx,p.Bundle); if err != nil { return p,err }
 var metadata archive.Metadata
 if err = json.Unmarshal(p.MetadataBytes,&metadata); err != nil { return p,err }
 refs,err := metadata.SourceReferences(); if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() { return p,ErrDurableStorageRecovery }
 q.Core.SourceSetSHA256,err = s.readyDigest(ctx,refs); if err != nil { return p,err }
 p.JournalVersion = 3; p.StagedSelection = q
 q.Core.BindingSHA256,err = s.readyBinding(ctx,p); if err != nil { return p,err }
 q.SHA256,err = s.readyDigest(ctx,*q); if err != nil { return p,err }
 return p,s.validateStagedSelection(ctx,p)
}

func (s *Store) validateStagedSelection(ctx context.Context,p PendingPublication) error {
 if p.StagedSelection == nil { return nil }
 if err := p.validateStagedShape(); err != nil { return err }
 q := p.StagedSelection
 cfg,reg,stage,err := s.stagedContext(ctx,p.Bundle.ArchiveSessionID); if err != nil { return err }
 if cfg.DestinationID() != q.Core.DestinationID || stage.SHA256 != q.StageSHA256 || !readySelectionOwnerMatches(reg,p.Bundle) { return ErrDurableStorageRecovery }
 checks := []struct{value any; want string}{{reg,q.Core.AdmissionContext},{stage.Children,q.RosterSHA256},{p.Bundle,q.Core.BundleSHA256},{struct{Filter,Adapter,Version,Skill string}{p.Bundle.Capture.FilterVersion,p.Bundle.Capture.AdapterName,p.Bundle.Capture.AdapterVersion,p.SkillEvidence},q.Core.PolicyContext}}
 for _,check := range checks { got,err := s.readyDigest(ctx,check.value); if err != nil || got != check.want { return errors.Join(ErrDurableStorageRecovery,err) } }
 binding,err := s.readyBinding(ctx,p); if err != nil || binding != q.Core.BindingSHA256 || readySHA(p.MetadataBytes) != q.Core.MetadataSHA256 { return errors.Join(ErrDurableStorageRecovery,err) }
 var metadata archive.Metadata
 if err = json.Unmarshal(p.MetadataBytes,&metadata); err != nil { return err }
 refs,err := metadata.SourceReferences(); if err != nil || len(refs) != 1 || refs[0] != p.SourceReference() { return ErrDurableStorageRecovery }
 got,err := s.readyDigest(ctx,refs); if err != nil || got != q.Core.SourceSetSHA256 { return errors.Join(ErrDurableStorageRecovery,err) }
 return nil
}

func (p *Published) saveStagedPublication(pending PendingPublication,at time.Time) error {
 if err := pending.ValidateReadySlot(p.id); err != nil { return err }
 if err := p.store.validateStagedSelection(p.store.durableContext(),pending); err != nil { return err }
 source := pending.SourceReference()
 next,err := p.nextBudgeted(pending.Bundle,at,CacheStatusPublished,"",pending.MetadataBytes,nil,&source); if err != nil { return err }
 proof := *pending.StagedSelection
 next.LastPublished.StagedSelection = &proof; next.PublicationVersion = 3
 return p.write(next)
}

func (p *Published) preserveStagedPublished(next *publishedState) error {
 prior := p.state.LastPublished
 if prior != nil && prior.StagedSelection != nil {
  if next.LastPublished == nil { return ErrReadySelectionFrozen }
  if next.LastPublished.StagedSelection == nil {
   if readySHA(next.MetadataBytes) != prior.StagedSelection.Core.MetadataSHA256 { return ErrReadySelectionFrozen }
   proof := *prior.StagedSelection; next.LastPublished.StagedSelection = &proof
  }
 }
 if next.LastPublished == nil || next.LastPublished.StagedSelection == nil { return nil }
 return p.validateStagedPublished(next)
}

func (p *Published) validateStagedPublished(next *publishedState) error {
 q := next.LastPublished.StagedSelection
 if next.LastPublished.Selection != nil || q.validateDigest() != nil || readySHA(next.MetadataBytes) != q.Core.MetadataSHA256 { return ErrDurableStorageRecovery }
 cfg,err := p.store.inspectDurableReadRoots(); if err != nil || !cfg.SupportsStagedSelection() || cfg.DestinationID() != q.Core.DestinationID { return errors.Join(ErrDurableStorageRecovery,err) }
 bundle,err := p.store.readyDigest(p.store.durableContext(),next.LastPublished.Bundle); if err != nil || bundle != q.Core.BundleSHA256 { return errors.Join(ErrDurableStorageRecovery,err) }
 var metadata archive.Metadata
 if err := json.Unmarshal(next.MetadataBytes,&metadata); err != nil { return ErrDurableStorageRecovery }
 refs,err := metadata.SourceReferences()
 if err != nil || len(refs) != 1 || next.LastPublished.Source == nil || refs[0] != *next.LastPublished.Source || metadata.SessionID != p.id || metadata.NativeChild != next.LastPublished.Bundle.NativeChild { return ErrDurableStorageRecovery }
 set,err := p.store.readyDigest(p.store.durableContext(),refs)
 if err != nil || set != q.Core.SourceSetSHA256 { return errors.Join(ErrDurableStorageRecovery,err) }
 reg,found,err := p.store.LoadRegistration(p.id)
 if err != nil || !found || reg.StagedImportSHA256 != q.StageSHA256 || !readySelectionOwnerMatches(reg,next.LastPublished.Bundle) { return errors.Join(ErrDurableStorageRecovery,err) }
 admission,err := p.store.readyDigest(p.store.durableContext(),reg)
 if err != nil || admission != q.Core.AdmissionContext { return errors.Join(ErrDurableStorageRecovery,err) }
 next.PublicationVersion = 3
 return nil
}

func (s *Store) checkStagedPendingRemoval(pending PendingPublication) error {
 published,err := s.LoadPublishedState(pending.Bundle.ArchiveSessionID); if err != nil { return err }
 if published.state.LastPublished == nil || published.state.LastPublished.StagedSelection == nil || *published.state.LastPublished.StagedSelection != *pending.StagedSelection { return ErrReadySelectionFrozen }
 return s.requireStagedReleaseProgress(pending.Bundle.ArchiveSessionID,pending.StagedSelection)
}

// Closed typed authority uses exact key spelling and rejects null/missing keys.
func strictStagedKeys(raw []byte,keys []string) error {
 if err := rejectDuplicateAuthorityKeys(raw,keys); err != nil { return err }
 var fields map[string]json.RawMessage
 if err := json.Unmarshal(raw,&fields); err != nil || fields == nil { return ErrDurableStorageRecovery }
 for _,key := range keys {
  value,present := fields[key]
  if !present || bytes.Equal(bytes.TrimSpace(value),[]byte("null")) { return ErrDurableStorageRecovery }
 }
 if len(fields) != len(keys) { return ErrDurableStorageRecovery }
 return nil
}

func rejectStagedNullAliases(raw []byte,names []string) error {
 if err := rejectDuplicateAuthorityKeys(raw,names); err != nil { return err }
 var fields map[string]json.RawMessage
 if json.Unmarshal(raw,&fields) != nil { return ErrDurableStorageRecovery }
 for key,value := range fields {
  for _,name := range names {
   if strings.EqualFold(key,name) && (key != name || bytes.Equal(bytes.TrimSpace(value),[]byte("null"))) { return ErrDurableStorageRecovery }
  }
 }
 return nil
}
