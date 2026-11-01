from pathlib import Path
import subprocess,os,json,time,hashlib
root=Path.cwd();base=Path('/tmp/goagent-all-examples-20261101-r1');(base/'bin-fixed').mkdir(exist_ok=True)
env=os.environ.copy();env.pop('GOROOT',None);env['GOTOOLCHAIN']='go1.25.0';env['GEMINI_MODEL']=env.get('GEMINI_MODEL') or 'gemini-2.5-flash';env['GOAGENT_LIVE_PROVIDER']='gemini'
secrets=[v for k,v in env.items() if any(w in k.upper() for w in ['API_KEY','TOKEN','SECRET']) and len(v)>8]
def safe(s):
 for v in secrets:s=s.replace(v,'[REDACTED]')
 return s
rows=[]
for leaf in ['parallel-diagnostics','review-loop']:
 p='orchestration/live/'+leaf;binary=base/'bin-fixed'/leaf
 build=subprocess.run(['go','build','-o',str(binary),'./'+p],env=env,cwd=root,capture_output=True,text=True,timeout=120)
 if build.returncode:raise RuntimeError(safe(build.stderr))
 start=time.time();args=['-dir',str(base/'state'/('fixed-'+leaf))]
 result=subprocess.run([str(binary)]+args,env=env,cwd=root,capture_output=True,text=True,timeout=300)
 log='logs/fixed-'+leaf+'.log';(base/log).write_text(safe(result.stdout+'\n'+result.stderr))
 rows.append({'entry':p,'args':args,'exit_code':result.returncode,'passed':result.returncode==0,'elapsed_seconds':time.time()-start,'source_sha256':hashlib.sha256((root/p/'main.go').read_bytes()).hexdigest(),'log':log})
 (base/'cli-retests.json').write_text(json.dumps(rows,indent=2))
 print(p,result.returncode,flush=True)
