import concurrent.futures, json, os, pathlib, subprocess, time
ROOT=pathlib.Path.cwd(); OUT=pathlib.Path('/tmp/goagent-all-examples-20261101-r1')
(OUT/'bin').mkdir(exist_ok=True);(OUT/'logs').mkdir(exist_ok=True);(OUT/'state').mkdir(exist_ok=True);(OUT/'outputs').mkdir(exist_ok=True)
ENV=os.environ.copy();ENV.pop('GOROOT',None);ENV['GOTOOLCHAIN']='go1.25.0';ENV['GOAGENT_LIVE_PROVIDER']='gemini';ENV['GEMINI_MODEL']=ENV.get('GEMINI_MODEL') or 'gemini-2.5-flash';ENV['MINIMAX_BASE_URL']=ENV.get('MINIMAX_BASE_URL') or 'https://api.minimax.cn/v1'
for k in ['GOAGENT_LIVE','GOAGENT_NATIVE_LIVE','GOAGENT_BUSINESS_LIVE']:ENV.pop(k,None)
SECRETS=[v for k,v in ENV.items() if any(w in k.upper() for w in ['API_KEY','TOKEN','SECRET']) and len(v)>8]
def safe(s):
 for v in SECRETS:s=s.replace(v,'[REDACTED]')
 return s
entries=sorted(str(p.parent.relative_to(ROOT)) for p in ROOT.rglob('main.go'))
records=[];binaries={}
def build(p):
 name=p.replace('/','__');dest=OUT/'bin'/name
 result=subprocess.run(['go','build','-o',str(dest),'./'+p],cwd=ROOT,env=ENV,capture_output=True,text=True,timeout=120)
 (OUT/'logs'/(name+'-build.log')).write_text(safe(result.stdout+result.stderr))
 return p,str(dest),result.returncode
with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
 for p,b,code in pool.map(build,entries):
  if code:raise RuntimeError('build failed: '+p)
  binaries[p]=b
(OUT/'builds.json').write_text(json.dumps({'entries':entries,'built':len(binaries)},indent=2))
print('BUILD all',len(binaries),'entries passed',flush=True)
def run(p,args=None,label=None,expected=(0,)):
 args=args or [];label=label or p.replace('/','__');cmd=[binaries[p]]+list(map(str,args));start=time.time()
 try:
  res=subprocess.run(cmd,cwd=ROOT,env=ENV,capture_output=True,text=True,timeout=300)
  code=res.returncode;stdout=safe(res.stdout);stderr=safe(res.stderr)
 except subprocess.TimeoutExpired as e:
  code=124;stdout=safe((e.stdout or b'').decode() if isinstance(e.stdout,bytes) else e.stdout or '');stderr='case runner 300s timeout'
 (OUT/'logs'/(label+'.log')).write_text(stdout+'\n'+stderr)
 rec={'entry':p,'label':label,'args':args,'exit_code':code,'expected_exit_codes':list(expected),'passed':code in expected,'elapsed_seconds':round(time.time()-start,3),'log':'logs/'+label+'.log'}
 records.append(rec);temp=OUT/'cli-results.tmp';temp.write_text(json.dumps(records,indent=2));temp.replace(OUT/'cli-results.json')
 print(('PASS' if rec['passed'] else 'FAIL'),label,'exit='+str(code),flush=True)
 return code,stdout
# Offline entry points and explicit variants.
for p in ['agent-usage','agent-usage/task','orchestration/agent-tool','orchestration/field-adapter','orchestration/parallel-diagnostics','orchestration/runnable-tool','orchestration/stream-pipeline']:run(p)
run('agent-usage/agent',['-mode','single'],'agent-single');run('agent-usage/agent',['-mode','child'],'agent-child')
for p in ['orchestration/composed-order','orchestration/context-recovery']:run(p,[str(OUT/'state'/p.replace('/','__'))])
# Business modes run real models; independent fixtures avoid accidentally replaying another mode.
for case in ['devops','article-review','auto-reply']:
 args=['-case',case,'-dir',str(OUT/'state'/('business-'+case)),'-live','-provider','gemini','-model',ENV['GEMINI_MODEL']]
 code,body=run('workflow/business-cases',args,'business-'+case)
 if code==0:
  try:
   obj=json.loads(body);snap=obj.get('snapshot',{});key=snap.get('Key') or snap.get('key')
   if obj.get('status')=='approval_required' and key:
    run('workflow/business-cases',args+['-approve',key,'-actor','demo-operator' if case=='devops' else 'demo-reviewer'],'business-'+case+'-approve')
  except ValueError:pass
# Both refund engines: approve, deliberate crash 86, recover, and replay.
for engine in ['steps','graph']:
 args=['-engine',engine,'-dir',str(OUT/'state'/('refund-'+engine))]
 code,body=run('workflow/refund-recovery',args,'refund-'+engine+'-waiting')
 if code==0:
  try:
   snap=json.loads(body).get('snapshot',{});key=snap.get('Key') or snap.get('key')
   if key:
    run('workflow/refund-recovery',args+['-approve',key,'-approver','demo-reviewer','-crash-after-refund'],'refund-'+engine+'-crash',(86,))
    run('workflow/refund-recovery',args,'refund-'+engine+'-resume');run('workflow/refund-recovery',args,'refund-'+engine+'-replay')
  except ValueError:pass
# All nine real-model orchestration CLI entries.
for leaf in ['approval','delegation','localize','log-triage','parallel-diagnostics','review-loop','session-replay','stream-budget','ticket-triage']:
 p='orchestration/live/'+leaf;state=str(OUT/'state'/('live-'+leaf))
 if leaf=='stream-budget':args=[]
 elif leaf=='session-replay':args=['-dir',state,'-ask','0']
 else:args=['-dir',state]
 code,_=run(p,args)
 if leaf=='session-replay' and code==0:
  run(p,['-dir',state,'-ask','1'],'session-ask1');run(p,['-dir',state,'-replay','0'],'session-replay0');run(p,['-dir',state,'-export',str(OUT/'outputs'/'session.jsonl')],'session-export')
# Five real ops CLIs.
for leaf in ['alert-rca','capacity-review','cost-attribution','scoped-lookup','sql-review']:
 p='orchestration/ops/'+leaf;run(p,['-dir',str(OUT/'state'/('ops-'+leaf))])
# Every usage live scenario independently: one failing model cannot skip later scenarios.
for scenario in ['single','child','task']:
 run('agent-usage/live',['-provider','gemini','-model',ENV['GEMINI_MODEL'],'-scenario',scenario,'-thinking-budget','128','-out',str(OUT/'outputs'/('usage-'+scenario))],'usage-live-'+scenario)
for case in ['mixed-replay','tool-error','cancel']:
 run('agent-usage/cases',['-case',case,'-out',str(OUT/'outputs'/('boundary-'+case))],'usage-boundary-'+case)
covered={r['entry'] for r in records};missing=sorted(set(entries)-covered)
print('COVERAGE',len(covered),'/',len(entries),'missing=',missing,'failed=',sum(not r['passed'] for r in records),flush=True)
(OUT/'coverage.json').write_text(json.dumps({'entries':entries,'covered':sorted(covered),'missing':missing,'runs':len(records),'failed':sum(not r['passed'] for r in records)},indent=2))
