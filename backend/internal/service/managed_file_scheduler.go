package service

import (
 "context"
 "fmt"
 "net/url"
 "strings"
	"sort"
 "time"
 "github.com/MACOS-DO/sub4api/internal/config"
 "github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
 "github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
)

const OpenAIEndpointCapabilityBPSFiles OpenAIEndpointCapability = "bps_files"

func validateOutboundURLWithConfig(cfg *config.Config, raw string) (string,error) {
 return (&OpenAIGatewayService{cfg:cfg}).validateOutboundURL(raw)
}

func BPSFilesConfigForAccount(a *Account) (BPSFilesBackendConfig,error) {
 var c BPSFilesBackendConfig
 if a==nil || a.Platform!=PlatformOpenAI || a.Type!=AccountTypeAPIKey || a.getExtraBool("openai_quality_guard_enabled") || a.getExtraBool("openai_raw_relay") || a.IsOpenAIPassthroughEnabled() || a.IsOpenAIResponsesWebSocketV2Enabled() || openai_compat.ResolveResponsesSupport(a.Extra)!=openai_compat.ResponsesSupportYes {return c,fmt.Errorf("managed files: unsupported account route")}
 caps,_:=a.openAIEndpointCapabilitySet()
 if !caps[string(OpenAIEndpointCapabilityBPSFiles)] || !a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) || a.GetCredential("bps_files_contract")!=managedfiles.Contract || strings.TrimSpace(a.GetCredential("base_url"))=="" || strings.TrimSpace(a.GetCredential("api_key"))=="" {return c,fmt.Errorf("managed files: missing explicit backend contract")}
 c=BPSFilesBackendConfig{ControlURL:a.GetCredential("bps_files_control_url"),ServiceKey:a.GetCredential("bps_files_service_key"),Expected:managedfiles.Identity{BackendID:a.GetCredential("bps_files_backend_id"),SubjectFingerprint:a.GetCredential("bps_files_subject_fingerprint"),CredentialGeneration:a.GetCredential("bps_files_credential_generation")},AccountID:a.ID,Concurrency:a.Concurrency}
 u,err:=url.Parse(c.ControlURL)
 if err!=nil || u.Hostname()=="" || (u.Scheme!="https" && u.Scheme!="http") || u.User!=nil || u.RawQuery!="" || u.ForceQuery || u.Fragment!="" || strings.TrimSpace(c.ControlURL)!=c.ControlURL || !bpsFilesOpaque(c.ServiceKey,4096) || managedfiles.ValidateIdentity(c.Expected)!=nil || c.AccountID<=0 || c.Concurrency<=0 {return BPSFilesBackendConfig{},fmt.Errorf("managed files: invalid backend configuration")}
 if a.Proxy!=nil {c.ProxyURL=a.Proxy.URL()}
 return c,nil
}

func (s *OpenAIGatewayService) RecheckManagedFileAccount(ctx context.Context,groupID *int64,accountID int64,model string)(*Account,error){
 if s==nil || s.accountRepo==nil || groupID==nil || *groupID<=0 || accountID<=0 || model=="" {return nil,ErrNoAvailableAccounts}
 ctx=context.WithValue(ctx,openAIProxyStreamQuarantineBypassKey{},false)
 if err:=ctx.Err();err!=nil{return nil,err}
 a,err:=s.accountRepo.GetByID(ctx,accountID)
 if err!=nil || a==nil || !a.IsSchedulable() || (a.ExpiresAt!=nil && !time.Now().Before(*a.ExpiresAt)) || !openAIStickyAccountMatchesGroup(a,groupID) || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx,a){return nil,ErrNoAvailableAccounts}
 if session,ok:=ManagedDeploymentSessionFromContext(ctx);ok {if session.GroupID!=*groupID || ValidateManagedDeploymentAccount(a,session.Binding)!=nil{return nil,ErrManagedDeploymentConflict}}
 if parent:=s.resolveOpenAIGuardianParentAccountID(ctx,groupID);parent>0 && parent!=accountID{return nil,ErrNoAvailableAccounts}
 if _,err=BPSFilesConfigForAccount(a);err!=nil{return nil,ErrNoAvailableAccounts}
 checker:=&defaultOpenAIAccountScheduler{service:s}
 req:=OpenAIAccountScheduleRequest{GroupID:groupID,Platform:PlatformOpenAI,RequestedModel:model,RequiredTransport:OpenAIUpstreamTransportHTTPSSE,RequiredCapability:OpenAIEndpointCapabilityResponses}
 if ok,_:=checker.isAccountRequestCompatibleReason(ctx,a,req);!ok{return nil,ErrNoAvailableAccounts}
 return a,nil
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForManagedFiles(ctx context.Context,groupID *int64,sessionHash,model string,requiredID int64)(*AccountSelectionResult,OpenAIAccountScheduleDecision,error){
 d:=OpenAIAccountScheduleDecision{Layer:"managed_files"}
 if requiredID<0{return nil,d,ErrNoAvailableAccounts}
 if session,ok:=ManagedDeploymentSessionFromContext(ctx);ok {
  if requiredID!=0 && requiredID!=session.Binding.AccountID{return nil,d,ErrManagedFileBindingConflict}
  requiredID=session.Binding.AccountID
 }
 if requiredID==0 {
  if s==nil || s.accountRepo==nil{return nil,d,ErrNoAvailableAccounts}
  rows,err:=s.accountRepo.ListModelAvailabilityCandidates(ctx,groupID,[]string{PlatformOpenAI},false)
  if err!=nil{return nil,d,err}
  sort.SliceStable(rows,func(i,j int)bool {if rows[i].Priority!=rows[j].Priority{return rows[i].Priority<rows[j].Priority};return rows[i].ID<rows[j].ID})
  for _,row:=range rows {
   if _,err:=BPSFilesConfigForAccount(&row);err!=nil{continue}
   selected,next,err:=s.SelectAccountWithSchedulerForManagedFiles(ctx,groupID,"",model,row.ID)
   if err==nil{return selected,next,nil}
  }
  return nil,d,ErrNoAvailableAccounts
 }
 a,err:=s.RecheckManagedFileAccount(ctx,groupID,requiredID,model);if err!=nil{return nil,d,err}
 before,beforeErr:=BPSFilesConfigForAccount(a);oldKey,oldURL:=a.GetCredential("api_key"),a.GetCredential("base_url")
 acquired,err:=s.tryAcquireAccountSlot(ctx,a.ID,a.Concurrency);if err!=nil{return nil,d,err}
 if acquired==nil || !acquired.Acquired{return nil,d,ErrNoAvailableAccounts}
 latest,err:=s.RecheckManagedFileAccount(ctx,groupID,requiredID,model)
 after,afterErr:=BPSFilesConfigForAccount(latest)
 if err!=nil || beforeErr!=nil || afterErr!=nil || before!=after || latest.GetCredential("api_key")!=oldKey || latest.GetCredential("base_url")!=oldURL{if acquired.ReleaseFunc!=nil{acquired.ReleaseFunc()};return nil,d,ErrNoAvailableAccounts}
 d.SelectedAccountID=a.ID;d.SelectedAccountType=a.Type
 return &AccountSelectionResult{Account:latest,Acquired:true,ReleaseFunc:acquired.ReleaseFunc},d,nil
}
