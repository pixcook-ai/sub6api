package service

import (
 "context"
 "github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
)

func (s *OpenAIGatewayService) AuthorizeManagedFileAccount(ctx context.Context,groupID int64,model string,selected *Account)(*Account,error){
 return s.authorizeManagedBPSAccount(ctx,groupID,model,selected,false)
}
func (s *OpenAIGatewayService) AuthorizeStoredResponseAccount(ctx context.Context,groupID int64,model string,selected *Account)(*Account,error){
 return s.authorizeManagedBPSAccount(ctx,groupID,model,selected,true)
}
func (s *OpenAIGatewayService) authorizeManagedBPSAccount(ctx context.Context,groupID int64,model string,selected *Account,stored bool)(*Account,error){
 if selected==nil || groupID<=0 || model=="" {return nil,ErrBPSRequestAuthorizationRevoked}
 if err:=CheckBPSRequestAuthorization(ctx,model);err!=nil{return nil,err}
 if s==nil || s.schedulerSnapshot==nil || s.schedulerSnapshot.groupRepo==nil {return nil,ErrBPSRequestAuthorizationUnavailable}
 group,err:=s.schedulerSnapshot.groupRepo.GetByID(ctx,groupID)
 if err!=nil{return nil,ErrBPSRequestAuthorizationUnavailable}
 if group==nil || group.ID!=groupID || !group.IsActive() || !group.ModelAllowlist.Allows(model){return nil,ErrBPSRequestAuthorizationRevoked}
 fresh:=context.WithValue(ctx,ctxkey.Group,group)
 fresh=context.WithValue(fresh,openAIProfitControlGateCtxKey{},(*openAIProfitControlGate)(nil))
 var current *Account
 if stored {current,err=s.RecheckBPSStoredResponseAccount(fresh,&groupID,selected.ID,model)} else {current,err=s.RecheckManagedFileAccount(fresh,&groupID,selected.ID,model)}
 if err!=nil || current==nil || current.GetCredential("api_key")!=selected.GetCredential("api_key") || current.GetCredential("base_url")!=selected.GetCredential("base_url"){return nil,ErrBPSRequestAuthorizationRevoked}
 if stored {before,e1:=BPSStoredResponsesConfigForAccount(selected);after,e2:=BPSStoredResponsesConfigForAccount(current);if e1!=nil || e2!=nil || before!=after{return nil,ErrBPSRequestAuthorizationRevoked}} else {before,e1:=BPSFilesConfigForAccount(selected);after,e2:=BPSFilesConfigForAccount(current);if e1!=nil || e2!=nil || before!=after{return nil,ErrBPSRequestAuthorizationRevoked}}
 for _,name:=range []string{model} {for _,compact:=range []bool{false,true}{oldBill,oldUp:=resolveOpenAIForwardMappedModels(selected,name,compact);newBill,newUp:=resolveOpenAIForwardMappedModels(current,name,compact);if oldBill!=newBill || oldUp!=newUp{return nil,ErrBPSRequestAuthorizationRevoked}}}
 if err:=ctx.Err();err!=nil{return nil,err}
 return current,nil
}
