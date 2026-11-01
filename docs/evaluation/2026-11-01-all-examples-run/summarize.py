from pathlib import Path
import json
b=Path(__file__).parent
rows=json.loads((b/'cli-results.json').read_text());retests=json.loads((b/'cli-retests.json').read_text())
entries=json.loads((b/'builds.json').read_text())['entries']
failures=[r for r in rows if not r['passed']]
fixes={r['entry']:r for r in retests if r['passed']}
final=[]
for entry in entries:
 executions=[r for r in rows if r['entry']==entry]
 unresolved=[r for r in executions if not r['passed'] and entry not in fixes]
 final.append({'entry':entry,'executions':len(executions),'initial_failures':len([r for r in executions if not r['passed']]),'retest':fixes.get(entry),'passed':bool(executions) and not unresolved})
assert len(final)==28 and all(r['passed'] for r in final),final
live={};initial=[]
for f in ['live-tests.jsonl','live-root-retest.jsonl']:
 for line in (b/f).read_text().splitlines():
  try:r=json.loads(line)
  except ValueError:continue
  if r.get('Action') in ['pass','fail','skip'] and r.get('Test') and '/' not in r['Test']:
   key=r['Package']+'::'+r['Test'];live[key]=r['Action']
   if r['Action']=='fail':initial.append(key)
assert len(live)==15 and all(v=='pass' for v in live.values()),live
native=json.loads((b/'native-loop.json').read_text());assert native['passed']
business=json.loads((b/'business-suite.json').read_text());assert len(business['results'])==6 and business['live_model'] is True
assert '--- PASS: TestLiveBusinessCases' in (b/'business-suite.log').read_text()
assert '--- PASS: TestLiveNativeToolLoop' in (b/'native-loop.log').read_text()
summary={'cli_entry_count':len(entries),'cli_executions':len(rows),'cli_retests':len(retests),'initial_cli_failures':failures,'final_cli':final,'real_model_tests':live,'initial_real_test_failures':initial,'business_real_cases':len(business['results']),'native_loop_passed':native['passed'],'native_loop_calls':native['model_calls_by_phase'],'provider_scope':'Gemini for ordinary real-model cases; Gemini+DeepSeek+MiniMax for mixed provider usage case','live_business_report':'business-suite.json','native_report':'native-loop.json'}
(b/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')
print('Verified',len(final),'CLI entries,',len(rows),'CLI runs +',len(retests),'retests;',len(live),'real model tests, 6 real business fixtures and native loop passed.')
