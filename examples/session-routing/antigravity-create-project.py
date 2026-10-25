#!/Library/Frameworks/Python.framework/Versions/3.12/bin/python3.12
"""Operator-owned native project provisioner. Never receives or replays task bodies."""
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

HOME=Path.home()
API=HOME/'.gemini/antigravity/bin/agentapi'
REGISTRY=HOME/'.config/tincan/host-session-routes.json'
STATE=HOME/'.config/tincan/project-creation'
SID=re.compile(r'[0-9a-f]{8}-[0-9a-f-]{27,40}\Z')


def runtime_context():
    rows=subprocess.run(['ps','-axo','pid=,command='],capture_output=True,text=True,check=True).stdout.splitlines()
    expected=str(HOME/'.gemini/config/sidecars/tincan-desktop-routing/desktop_listener.py')+' manager'
    for line in rows:
        if not line.endswith(expected):continue
        pid=int(line.split(None,1)[0])
        raw=subprocess.run(['ps','eww','-p',str(pid),'-o','command='],capture_output=True,text=True,check=True).stdout
        env={}
        for key in ('ANTIGRAVITY_LS_ADDRESS','ANTIGRAVITY_CSRF_TOKEN'):
            match=re.search(r'(?:^|\s)'+key+r'=([^\s]+)',raw)
            if match:env[key]=match.group(1)
        if env.get('ANTIGRAVITY_LS_ADDRESS'):return env
    raise RuntimeError('existing_native_sidecar_context_unavailable')


def rpc(env,method,body):
    address=env['ANTIGRAVITY_LS_ADDRESS']
    if not address.startswith('http'):address='http://'+address
    request=urllib.request.Request(address+'/exa.language_server_pb.LanguageServerService/'+method,
        data=json.dumps(body).encode(),headers={'Content-Type':'application/json','x-codeium-csrf-token':env.get('ANTIGRAVITY_CSRF_TOKEN','')})
    with urllib.request.urlopen(request,timeout=8) as response:return json.load(response)


def save(path,value):
    fd,tmp=tempfile.mkstemp(dir=path.parent,prefix='.creation-')
    try:
        os.fchmod(fd,0o600)
        with os.fdopen(fd,'w') as f:
            json.dump(value,f);f.flush();os.fsync(f.fileno())
        os.replace(tmp,path)
    finally:
        if os.path.exists(tmp):os.unlink(tmp)


def metadata(env,sid):
    result=subprocess.run([str(API),'get-conversation-metadata',sid],env={**os.environ,**env},capture_output=True,text=True,timeout=8,check=True)
    value=json.loads(result.stdout)
    return value['response']['conversationMetadata']['metadata']


def verify(env,sid,project):
    value=metadata(env,sid)
    if (value.get('rootConversationId')!=sid or value.get('parentConversationId')!=''
            or value.get('workspaceUris')!=[Path(project).as_uri()]):
        raise RuntimeError('native_project_identity_mismatch')


def ensure(request):
    if request.get('host') != 'antigravity':
        spec = importlib.util.spec_from_file_location('native_project_create', Path(__file__).with_name('project-create.py'))
        adapter = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(adapter)
        return adapter.ensure(request)
    if request.get('agent')!='antigravity' or request.get('host')!='antigravity':
        raise RuntimeError('unsupported_operator_native_host')
    project=request.get('project','')
    path=Path(project)
    if not path.is_absolute() or str(path.resolve(strict=True))!=project or not path.is_dir():
        raise RuntimeError('canonical_existing_project_required')
    STATE.mkdir(mode=0o700,parents=True,exist_ok=True)
    if STATE.is_symlink() or STATE.stat().st_uid!=os.getuid() or STATE.stat().st_mode&0o077:
        raise RuntimeError('private_creation_state_required')
    key=hashlib.sha256(('antigravity\0'+project).encode()).hexdigest()
    fd=os.open(STATE/(key+'.lock'),os.O_CREAT|os.O_RDWR|getattr(os,'O_NOFOLLOW',0),0o600)
    try:
        fcntl.flock(fd,fcntl.LOCK_EX)
        env=runtime_context()
        registry=json.loads(REGISTRY.read_text())
        owners=[r for r in registry['owners'] if (r['agent'],r['host'],r['project'])==('antigravity','antigravity',project)]
        if len(owners)>1:raise RuntimeError('ambiguous_native_owner')
        if owners:
            verify(env,owners[0]['session_id'],project)
            return owners[0]
        discovered=[r for r in registry['sessions'] if (r['agent'],r['host'],r['project'])==('antigravity','antigravity',project)]
        if len(discovered)>1:raise RuntimeError('ambiguous_discovered_sessions_no_creation')
        if discovered:
            verify(env,discovered[0]['session_id'],project)
            return discovered[0]
        receipt=STATE/(key+'.json')
        if receipt.exists():
            state=json.loads(receipt.read_text())
            if not state.get('session_id'):raise RuntimeError('prior_native_creation_uncertain_do_not_retry')
            verify(env,state['session_id'],project)
            if not any(r['session_id']==state['session_id'] and r['project']==project for r in registry['sessions']):
                raise RuntimeError('native_preinvocation_registration_pending')
            return {'agent':'antigravity','host':'antigravity','project':project,'session_id':state['session_id']}
        uri=path.as_uri()
        candidates=[]
        for pid in rpc(env,'ListProjects',{}).get('projectIds',[]):
            p=rpc(env,'ReadProject',{'id':pid}).get('project',{})
            resources=p.get('projectResources',{}).get('resources',[])
            folders=[r.get('gitFolder',r.get('folder',{})).get('folderUri') for r in resources]
            if folders==[uri]:candidates.append(pid)
        if len(candidates)>1:raise RuntimeError('ambiguous_native_project')
        if candidates:pid=candidates[0]
        else:
            pid=str(uuid.uuid5(uuid.NAMESPACE_URL,'tincan:antigravity:'+project))
            rpc(env,'CreateProject',{'project':{'id':pid,'name':path.name,'projectResources':{'resources':[{'gitFolder':{'folderUri':uri}}]}}})
            p=rpc(env,'ReadProject',{'id':pid}).get('project',{})
            resources=p.get('projectResources',{}).get('resources',[])
            if [r.get('gitFolder',{}).get('folderUri') for r in resources]!=[uri]:raise RuntimeError('native_project_not_persisted')
        save(receipt,{'phase':'native_creation_started','project':project,'native_project_id':pid})
        # Native model/grants stay under the project's existing settings.
        result=subprocess.run([str(API),'new-conversation','--title='+path.name,
            'Initialize this project session for Tincan. Do not edit files yet. Use the native PreInvocation hook to register the current session and workspace.'],
            cwd=project,env={**os.environ,**env,'ANTIGRAVITY_PROJECT_ID':pid},capture_output=True,text=True,timeout=15,check=True)
        ids=list(dict.fromkeys(re.findall(r'\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b',result.stdout)))
        if len(ids)!=1:raise RuntimeError('native_creation_result_uncertain')
        sid=ids[0]
        save(receipt,{'phase':'native_id_received','project':project,'native_project_id':pid,'session_id':sid})
        verify(env,sid,project)
        # The native invocation hook, not sender text or a fabricated ID, must register it.
        deadline=time.monotonic()+8
        while True:
            registry=json.loads(REGISTRY.read_text())
            rows=[r for r in registry['sessions'] if (r['agent'],r['host'],r['project'],r['session_id'])==('antigravity','antigravity',project,sid)]
            if len(rows)==1:break
            if time.monotonic()>=deadline:raise RuntimeError('native_preinvocation_registration_pending')
            time.sleep(0.2)
        save(receipt,{'phase':'verified','project':project,'native_project_id':pid,'session_id':sid})
        return rows[0]
    finally:os.close(fd)


if __name__=='__main__':
    try:
        raw=sys.stdin.buffer.read(8193)
        if len(raw)>8192:raise RuntimeError('request_too_large')
        print(json.dumps(ensure(json.loads(raw))))
    except Exception:
        # Native errors/tracebacks can contain credentials: emit a fixed error only.
        print('native_project_creation_failed_or_unconfirmed',file=sys.stderr)
        sys.exit(1)
