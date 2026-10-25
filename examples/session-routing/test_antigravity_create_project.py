import importlib.util,json,subprocess,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
spec=importlib.util.spec_from_file_location('creator',Path(__file__).with_name('antigravity-create-project.py'))
c=importlib.util.module_from_spec(spec);spec.loader.exec_module(c)
SID='11111111-2222-4333-8444-555555555555'
class CreatorTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name).resolve();self.project=self.root/'repo';self.project.mkdir()
  self.registry=self.root/'routes.json';self.registry.write_text(json.dumps({'sessions':[],'owners':[]}))
  self.req={'agent':'antigravity','host':'antigravity','project':str(self.project)}
  self.patches=[patch.object(c,'STATE',self.root/'state'),patch.object(c,'REGISTRY',self.registry),patch.object(c,'runtime_context',return_value={}),patch.object(c,'metadata',return_value={'rootConversationId':SID,'parentConversationId':'','workspaceUris':[self.project.as_uri()]})]
  for p in self.patches:p.start()
 def tearDown(self):
  for p in reversed(self.patches):p.stop()
  self.tmp.cleanup()
 def test_new_native_session_registered_and_reused(self):
  row={**self.req,'session_id':SID};calls=[]
  def rpc(env,method,body):
   calls.append(method)
   if method=='ListProjects':return {'projectIds':[]}
   if method=='ReadProject':return {'project':{'projectResources':{'resources':[{'gitFolder':{'folderUri':self.project.as_uri()}}]}}}
   return {}
  def native(*args,**kw):
   self.assertIn('ANTIGRAVITY_PROJECT_ID',kw['env']);self.assertEqual(kw['cwd'],str(self.project))
   self.registry.write_text(json.dumps({'owners':[row],'sessions':[row]}))
   return subprocess.CompletedProcess(args,0,stdout=SID)
  with patch.object(c,'rpc',side_effect=rpc),patch.object(c.subprocess,'run',side_effect=native) as run:
   self.assertEqual(c.ensure(self.req),row);self.assertEqual(c.ensure(self.req),row)
   self.assertEqual(run.call_count,1);self.assertEqual(calls.count('CreateProject'),1)
 def test_discovered_unpinned_session_reused_without_creation(self):
  row={**self.req,'session_id':SID}
  self.registry.write_text(json.dumps({'owners':[],'sessions':[row]}))
  with patch.object(c,'rpc') as rpc,patch.object(c.subprocess,'run') as run:
   self.assertEqual(c.ensure(self.req),row)
   rpc.assert_not_called();run.assert_not_called()
 def test_multiple_discoveries_never_create_or_choose_first(self):
  row={**self.req,'session_id':SID}
  self.registry.write_text(json.dumps({'owners':[],'sessions':[row,{**row,'session_id':'66666666-2222-4333-8444-555555555555'}]}))
  with patch.object(c.subprocess,'run') as run:
   with self.assertRaisesRegex(RuntimeError,'ambiguous'):c.ensure(self.req)
   run.assert_not_called()
 def test_uncertain_native_creation_never_retried(self):
  with patch.object(c,'rpc',return_value={'projectIds':['existing'],'project':{'projectResources':{'resources':[{'gitFolder':{'folderUri':self.project.as_uri()}}]}}}),patch.object(c.subprocess,'run',side_effect=subprocess.TimeoutExpired('native',15)) as run:
   with self.assertRaises(subprocess.TimeoutExpired):c.ensure(self.req)
   with self.assertRaisesRegex(RuntimeError,'uncertain'):c.ensure(self.req)
   self.assertEqual(run.call_count,1)
 def test_wrong_project_native_identity_rejected(self):
  with patch.object(c,'metadata',return_value={'rootConversationId':SID,'parentConversationId':'','workspaceUris':['file:///other']}):
   with self.assertRaisesRegex(RuntimeError,'identity'):c.verify({},SID,str(self.project))
if __name__=='__main__':unittest.main()
