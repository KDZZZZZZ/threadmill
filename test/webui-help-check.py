"""Generate a browser regression using the shipped UI and mocked HTTP/SSE.

Run: python3 test/webui-help-check.py /tmp/help-check/index.html
Serve that directory with python3 -m http.server, then open it in a browser.
No model or live project is used. PASS/FAIL appears over the actual UI.
"""

import pathlib
import sys

source = (pathlib.Path(__file__).resolve().parents[1] / "docs/webui-demo.html").read_text()
fixture = r'''
<script>
(() => {
 const roles=['planner','executor','verifier'],streams=[];
 const project={id:'help-check',name:'Help recovery',root:'/fixture/help-check',runtime_state:'open',busy:true,pending:3,manager_id:'manager'};
 const tasks=roles.map(role=>({ID:role,Outcome:'active',...Object.fromEntries(roles.map(r=>[r[0].toUpperCase()+r.slice(1),{ID:`${role}:1:${r}`}]))}));
 const agents=roles.map(role=>({id:`${role}:1:${role}`,task_id:role,role,state:'waiting',activity:'tool',current_tool:'coordination_requestHelp'}));
 const graph={tasks,edges:[]}; let sequence=0;
 const snapshot=()=>({project,messages:{items:[]},agents:{items:agents},graph});
 window.fetch=async path=>new Response(JSON.stringify(path==='/healthz'?{service:'threadmill-webui'}:path==='/api/v1/projects'?{items:[project]}:path.endsWith('/agents')?{items:agents}:path.endsWith('/graph')?graph:project),{status:200,headers:{'Content-Type':'application/json'}});
 function emit(stream,name,data){if(!stream.closed)stream.dispatchEvent(new MessageEvent(name,{data:JSON.stringify({project_id:project.id,seq:String(++sequence),data})}));}
 window.EventSource=class extends EventTarget {
  constructor(){super();streams.push(this);setTimeout(()=>{if(!this.closed){this.onopen?.();emit(this,'snapshot',snapshot());}},0);}
  close(){this.closed=true;}
 };
 const pause=ms=>new Promise(resolve=>setTimeout(resolve,ms));
 const waits=()=>document.querySelectorAll('.flow-event.waiting');
 addEventListener('load',async()=>{
  const results=document.createElement('pre');results.id='help-check-results';results.style='position:fixed;z-index:10000;top:0;left:0;margin:0;padding:12px;background:#18231d;color:#fff';document.body.append(results);
  let passed=0,failed=0;
  function check(name,ok){results.textContent+=`${ok?'PASS':'FAIL'} ${name}\n`;ok?passed++:failed++;}
  await pause(350);
  check('Snapshot restores all three role Help waits',waits().length===3);
  check('Waiting is not animated as running',document.querySelectorAll('.flow-event.running').length===0);
  await pause(1800);
  check('Metadata refresh preserves each wait exactly once',waits().length===3);
  for(const s of streams)emit(s,'snapshot',snapshot());await pause(100);
  check('Repeated snapshot does not duplicate waits',waits().length===3);
  document.querySelector('#project-list button').click();await pause(100);
  check('Project resubscription restores waits',waits().length===3);
  agents[0].state='idle';agents[0].current_tool=null;agents[0].activity='';
  for(const s of streams)emit(s,'runtime_event',{agent_id:agents[0].id,kind:'tool',name:'coordination_requestHelp',phase:'end'});await pause(100);
  check('Help completion clears only the requester wait',waits().length===2&&!Array.from(waits()).some(e=>e.dataset.agent===agents[0].id));
  agents[1].state='canceled';agents[1].current_tool=null;agents[1].activity='';
  for(const s of streams)emit(s,'snapshot',snapshot());await pause(100);
  check('Canceled wait clears on refreshed snapshot',waits().length===1);
  results.dataset.failures=failed;results.dataset.passed=passed;document.title=`${failed?'FAIL':'PASS'} Help recovery (${passed}/${passed+failed})`;
 });
})();
</script>
'''
output = pathlib.Path(sys.argv[1])
output.parent.mkdir(parents=True, exist_ok=True)
output.write_text(source.replace("<script", fixture + "<script", 1))
print(output)
