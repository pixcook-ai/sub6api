package service

import (
 "bytes"
 "context"
 "io"
 "net/http"
 "net/url"
 "strings"
 "github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
 "github.com/gin-gonic/gin"
)

// PrepareBPSResourceContext captures the authenticated conversation before any
// compatibility transform changes the public request.
func (s *OpenAIGatewayService) PrepareBPSResourceContext(ctx context.Context,c *gin.Context,account *Account,body []byte)(context.Context,error){
 return s.prepareBPSSessionBinding(ctx,c,account,body)
}

// PrepareManagedFileHTTPRequest runs after all wire transforms, before the
// single actual send. All remote upload outcomes are durable before it returns.
func (s *OpenAIGatewayService) PrepareManagedFileHTTPRequest(request *http.Request,account *Account)(*http.Request,error){
 if request==nil {return nil,&ManagedFilePreparationError{ErrManagedFileNotReady}}
 plan:=ManagedFilePlanFromContext(request.Context())
 if plan==nil {return request,nil}
 if s==nil || !s.managedFiles.Enabled() || account==nil || request.URL==nil || request.GetBody==nil || request.Header.Get("Content-Encoding")!="" {return nil,&ManagedFilePreparationError{ErrManagedFileNotReady}}
 expected,err:=BPSFilesConfigForAccount(account);if err!=nil{return nil,&ManagedFilePreparationError{err}}
 control,err:=url.Parse(expected.ControlURL)
 if err!=nil || request.URL.Scheme!=control.Scheme || request.URL.Host!=control.Host || (request.URL.Path!="/v1/responses" && request.URL.Path!="/responses"){return nil,&ManagedFilePreparationError{ErrManagedFileNotReady}}
 reader,err:=request.GetBody();if err!=nil{return nil,&ManagedFilePreparationError{err}}
 raw,readErr:=io.ReadAll(io.LimitReader(reader,managedfiles.MaxJSONBytes+1));closeErr:=reader.Close()
 if readErr!=nil || closeErr!=nil || len(raw)>managedfiles.MaxJSONBytes {return nil,&ManagedFilePreparationError{ErrManagedFileUnsupportedInput}}
 ctx:=request.Context()
 state:=bpsSessionBindingFrom(ctx)
 if state==nil || state.authorize==nil {return nil,&ManagedFilePreparationError{ErrBPSRequestAuthorizationUnavailable}}
 ctx=withManagedFilePreparationAuthorization(ctx,state.authorize)
 resolved,err:=s.managedFiles.Resolve(ctx,plan,account,raw);if err!=nil{return nil,&ManagedFilePreparationError{err}}
 if resolved==nil || resolved.Config!=expected{return nil,&ManagedFilePreparationError{ErrManagedFileNotReady}}
 ctx=withManagedFileGeneration(ctx,account.ID,resolved.Config.ServiceKey,resolved.AdmissionID,resolved.OwnerRef,resolved.GroupRef,resolved.BindingSHA256,resolved.Authorize)
 request=request.WithContext(ctx)
 request.Body=io.NopCloser(bytes.NewReader(resolved.Body));request.ContentLength=int64(len(resolved.Body));request.TransferEncoding=nil
 request.Header.Del("Content-Encoding");request.GetBody=nil
 prepareManagedFileGenerationRequest(request)
 return request,nil
}

// ClaimBPSResourceRequest is called exactly once immediately before HTTP Do.
func ClaimBPSResourceRequest(request *http.Request,account *Account)error{
 if request==nil || account==nil{return &ManagedFilePreparationError{ErrManagedFileNotReady}}
 if !isStoredResponseGeneration(request.Context()) {prepareManagedFileGenerationRequest(request)}
 if err:=prepareBPSSessionBindingSend(request,account);err!=nil{return err}
 if err:=claimManagedFileGeneration(request.Context(),account.ID);err!=nil{return err}
 return ClaimStoredResponseRequest(request.Context(),account.ID)
}

func IsBPSResourceRequest(ctx context.Context)bool{
 return ctx!=nil && (IsManagedFileGeneration(ctx)||isStoredResponseGeneration(ctx)||bpsSessionBindingFrom(ctx)!=nil)
}

// Public clients may never submit private admission or signed-session proof.
func StripBPSResourceHeaders(header http.Header){
 for name:=range header {if strings.HasPrefix(strings.ToLower(name),"x-bps-"){delete(header,name)}}
}
