#!/usr/bin/env python3
"""Exercise the actual managed-deployment SQL on an isolated, disposable PG17.
No ports, host database, upstream OAuth or model traffic. Uses an existing image.
"""
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import importlib.util,json,re,subprocess,time,uuid
BASE=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location("managed_oauth_pg",BASE/"scripts/test-managed-oauth-postgres.py")
helpers=importlib.util.module_from_spec(spec);spec.loader.exec_module(helpers)
run=helpers.run_docker;qt=helpers.sql_text;qj=helpers.sql_json
source=(BASE/"internal/repository/account_repo_managed_deployment.go").read_text()
def sql(name):
 matches=re.findall(r"const\s+"+name+r"\s*=\s*"+chr(96)+"([^"+chr(96)+"]+)"+chr(96),source)
 assert len(matches)==1,name
 return matches[0]
def require(ok,msg):
 if not ok:raise AssertionError(msg)
def main():
 name="sub6-deployment-test-"+uuid.uuid4().hex[:12]
 run("image","inspect","postgres:17-alpine")
 run("run","--rm","-d","--name",name,"--network","none","--tmpfs","/var/lib/postgresql/data:rw","-e","POSTGRES_PASSWORD=local-fixture","postgres:17-alpine")
 checks=0
 try:
  for _ in range(80):
   if run("exec",name,"pg_isready","-U","postgres",check=False).returncode==0:break
   time.sleep(.2)
  else:raise RuntimeError("isolated PostgreSQL did not start")
  preparation="SET statement_timeout='12s'; SET lock_timeout='8s';\n"
  for statement,params,const in [("put","text,bigint,bigint,text,jsonb,jsonb,bigint,bigint,bigint,text","putManagedDeploymentSQL"),("bind","bigint,bigint,bigint,text,boolean","bindManagedDeploymentSessionSQL"),("resource","text,bigint,bigint,text,text,text","bindManagedDeploymentResourceSQL"),("resolve","bigint,bigint,bigint,text,text","resolveManagedDeploymentResourceSQL")]:
   preparation+="PREPARE "+statement+"("+params+") AS "+sql(const)+";\n"
  def psql(statement,check=True):return run("exec","-i",name,"psql","-X","-qAt","-v","ON_ERROR_STOP=1","-U","postgres",sql=preparation+statement,check=check).stdout.strip()
  def raw(statement,check=True):return run("exec","-i",name,"psql","-X","-qAt","-v","ON_ERROR_STOP=1","-U","postgres",sql=statement,check=check)
  raw("CREATE TABLE groups(id bigint PRIMARY KEY,platform text,deleted_at timestamptz); CREATE TABLE users(id bigint PRIMARY KEY); CREATE TABLE api_keys(id bigint PRIMARY KEY); CREATE TABLE accounts(id bigint PRIMARY KEY,platform text,type text,extra jsonb,deleted_at timestamptz,credentials jsonb DEFAULT '{}'::jsonb,status text DEFAULT 'active',schedulable boolean DEFAULT TRUE,parent_account_id bigint,updated_at timestamptz DEFAULT NOW()); CREATE TABLE account_groups(account_id bigint,group_id bigint);"+(BASE/"migrations/251_managed_deployments.sql").read_text())
  def binding(provider,account):return {"provider":provider,"account_id":account,"backend_id":"worker-"+provider,"binding_ref":"bind_"+str(uuid.uuid4()),"profile_ref":"oauth_"+str(uuid.uuid4()),"identity_epoch":str(uuid.uuid4()),"workspace_id":"workspace-"+provider,"subject":"subject-"+provider}
  b=binding("bps_excel",11);c=binding("codex",12);bm=dict(b);bm.pop("account_id");cm=dict(c);cm.pop("account_id");cm["contract"]="tosub2-managed-oauth/v1";cm["token_revision"]=1
  dep="deployment_"+str(uuid.uuid4())
  raw("INSERT INTO groups VALUES(1,'openai',null),(2,'openai',null),(3,'openai',null); INSERT INTO users VALUES(1),(2); INSERT INTO api_keys VALUES(1),(2); INSERT INTO accounts(id,platform,type,extra,deleted_at) VALUES(11,'openai','apikey',"+qj({"managed_deployment_binding":bm})+",null),(12,'openai','oauth',"+qj({"managed_oauth":cm})+",null); INSERT INTO account_groups VALUES(11,1),(12,1),(11,2),(12,2);")
  def put(rev,provider,expected,group=1,dep_id=dep,bps=b,codex=c):return "EXECUTE put("+",".join([qt(dep_id),str(group),str(rev),qt(provider),qj(bps),qj(codex),str(bps["account_id"]),str(codex["account_id"]),str(expected),qt("tosub2-managed-oauth/v1")])+");"
  def bind(session,user=1,key=1,group=1,existing=False):return "EXECUTE bind("+",".join([str(group),str(user),str(key),qt(session),"true" if existing else "false"])+");"
  require(dep in psql(put(1,"bps_excel",0)),"create failed");checks+=1
  require(dep in psql(put(1,"bps_excel",0)),"idempotent create failed");checks+=1
  require(not psql(put(3,"codex",2)),"stale/nonexistent revision accepted");checks+=1
  different=dict(b);different["identity_epoch"]=str(uuid.uuid4());require(not psql(put(2,"codex",1,bps=different)),"mutable identity accepted");checks+=1
  require(not psql(put(1,"bps_excel",0,group=3,dep_id="deployment_"+str(uuid.uuid4()))),"nonmember accounts accepted");checks+=1
  conflict=raw(preparation+put(1,"bps_excel",0,dep_id="deployment_"+str(uuid.uuid4())),False);require(conflict.returncode!=0,"duplicate group deployment allowed");checks+=1
  first=psql(bind("old-session"));require('"provider": "bps_excel"' in first,"old session initial choice wrong");checks+=1
  require(dep in psql(put(2,"codex",1)),"switch failed");checks+=1
  require(psql(bind("old-session"))==first,"old session moved after switch");checks+=1
  new=psql(bind("new-session"));require('"provider": "codex"' in new,"new session ignored manual selection");checks+=1
  raw("UPDATE managed_deployment_sessions SET created_at='2000-01-01' WHERE session_key='old-session';")
  require(psql(bind("old-session"))==first,"old session expired into another backend");checks+=1
  require(not psql(bind("missing",existing=True)),"existing-only silently created binding");checks+=1
  require('"provider": "codex"' in psql(bind("old-session",user=2)),"different user reused original binding");checks+=1
  require('"provider": "codex"' in psql(bind("old-session",key=2)),"different API key reused original binding");checks+=1
  def resource(session):return "EXECUTE resource("+",".join([qt(dep),"1","1",qt("response"),qt("hash-response"),qt(session)])+");"
  def resolve(user=1,key=1,group=1):return "EXECUTE resolve("+",".join([str(group),str(user),str(key),qt("response"),qt("hash-response")])+");"
  psql(resource("old-session"));require('"provider": "bps_excel"' in psql(resolve()),"trusted resource owner lost");checks+=1
  psql(resource("new-session"));require('"provider": "bps_excel"' in psql(resolve()),"conflicting binding stole resource");checks+=1
  require(not psql(resolve(user=2)) and not psql(resolve(key=2)) and not psql(resolve(group=2)),"resource leaked across tenant or group");checks+=3
  # Rebind the same upstream resource in another session on the same identity:
  # retain the original owner instead of transferring or rejecting it.
  require(dep in psql(put(3,"bps_excel",2)),"switch back failed");psql(bind("same-identity"));psql(resource("same-identity"));require("old-session" in psql(resolve()),"same-binding reuse changed original owner");checks+=1
  # Distinct racing operations with the same CAS base: exactly one wins.
  with ThreadPoolExecutor(max_workers=2) as pool:result=list(pool.map(psql,[put(4,"codex",3),put(4,"bps_excel",3)]))
  require(sum(bool(v) for v in result)==1,"concurrent revision CAS admitted two different selections");checks+=1
  # Concurrent first requests all observe one permanent binding.
  with ThreadPoolExecutor(max_workers=12) as pool:bindings=list(pool.map(lambda _:psql(bind("concurrent-first")),range(24)))
  require(len(set(bindings))==1,"concurrent session creation returned mixed bindings");checks+=1
  count=raw("SELECT count(*) FROM managed_deployment_sessions WHERE session_key='concurrent-first';").stdout.strip();require(count=="1","concurrent duplicate sessions");checks+=1
  # Route selection and the first request serialize on the deployment row.
  for i in range(8):
   rev=int(raw("SELECT route_revision FROM managed_deployments WHERE group_id=1;").stdout.strip())
   with ThreadPoolExecutor(max_workers=2) as pool:results=list(pool.map(psql,[put(rev+1,"codex" if i%2 else "bps_excel",rev),bind("race-"+str(i))]))
   require(results[1] and psql(bind("race-"+str(i)))==results[1],"switch/first-session race changed established identity");checks+=1
  # Core-owned Codex has no external declaration. Its profile ref on the
  # account is the canonical UUID; durable deployment bindings prefix it.
  core=dict(c);core["workspace_id"]=b["workspace_id"];core["subject"]=b["subject"]
  binding_core={k:v for k,v in core.items() if k!="account_id"}
  marker={k:core[k] for k in ("provider","profile_ref","subject","workspace_id","identity_epoch")}
  marker.update(profile_ref=core["profile_ref"].removeprefix("oauth_"),contract="sub6-core-oauth/v1",token_revision=1,expires_at="2099-01-01T00:00:00Z")
  core_extra={"managed_deployment_binding":binding_core,"core_oauth":marker}
  core_dep="deployment_"+str(uuid.uuid4())
  raw("UPDATE accounts SET extra="+qj(core_extra)+" WHERE id=12;")
  require(core_dep in psql(put(1,"codex",0,group=2,dep_id=core_dep,codex=core)),"core owned Codex binding rejected");checks+=1
  raw("UPDATE accounts SET extra="+qj(dict(core_extra,managed_oauth=cm))+" WHERE id=12;")
  require(not psql(put(2,"bps_excel",1,group=2,dep_id=core_dep,codex=core)),"dual core/external authorities accepted");checks+=1
  raw("UPDATE accounts SET extra="+qj(core_extra)+",credentials="+qj({"refresh_token":"synthetic"})+" WHERE id=12;")
  require(not psql(put(2,"bps_excel",1,group=2,dep_id=core_dep,codex=core)),"worker refresh token accepted");checks+=1
  raw("UPDATE accounts SET credentials='{}'::jsonb WHERE id=12; UPDATE accounts SET schedulable=FALSE WHERE id=11;")
  require(not psql(put(2,"bps_excel",1,group=2,dep_id=core_dep,codex=core)),"manual pause overwritten by switch");checks+=1
  require(raw("SELECT route_revision||':'||selected_provider FROM managed_deployments WHERE group_id=2;").stdout.strip()=="1:codex","failed switch changed route");checks+=1
  print("PASS managed-deployment PostgreSQL: "+str(checks)+" checks (real SQL, CAS, tenant isolation, durable resources, concurrent sessions)")
 finally:run("rm","-f",name,check=False)
if __name__=="__main__":main()
