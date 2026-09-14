
(() => {
 const streams=[],project={id:'order-check',name:'Message order',root:'/fixture/order',runtime_state:'open',busy:true,pending:1};
 const messages=[{id:'u1',speaker:'user',content:'Original request'},{id:'a1',speaker:'manager',content:'Completed original request'},{id:'u2',speaker:'user',content:'Follow-up request',status:'queued'}];
 const reasoning={manager:{running:true,text:'Current reasoning',started_at:new Date().toISOString()}},graph={tasks:[],edges:[]};
 let sequence=0;
 const snapshot=()=>({project,messages:{items:messages},agents:[],graph,reasoning});
 window.fetch=async path=>new Response(JSON.stringify(path==='/healthz'?{service:'threadmill-webui'}:path==='/api/v1/projects'?{items:[project]}:path.endsWith('/messages')?{items:messages}:path.endsWith('/agents')?{items:[]}:path.endsWith('/graph')?graph:project),{headers:{'Content-Type':'application/json'}});
 function emit(name,data){for(const s of streams)if(!s.closed)s.dispatchEvent(new MessageEvent(name,{data:JSON.stringify({project_id:project.id,seq:String(++sequence),data})}));}
 window.EventSource=class extends EventTarget {constructor(){super();streams.push(this);setTimeout(()=>{this.onopen?.();emit('snapshot',snapshot());},0);}close(){this.closed=true;}};
 const pause=ms=>new Promise(resolve=>setTimeout(resolve,ms));
 const afterFollowup=()=>{
  const user=Array.from(document.querySelectorAll('.message.user')).find(x=>x.textContent.includes('Follow-up request'));
  const activity=document.querySelector('.loading-state');
  return !!(user&&activity&&(user.compareDocumentPosition(activity)&Node.DOCUMENT_POSITION_FOLLOWING));
 };
 addEventListener('load',async()=>{
  const results=document.createElement('pre');results.id='message-order-results';results.style='position:fixed;z-index:10000;top:0;left:0;margin:0;padding:12px;background:#18231d;color:#fff';document.body.append(results);
  let passed=0,failed=0;
  function check(name,ok){results.textContent+=`${ok?'PASS':'FAIL'} ${name}\n`;ok?passed++:failed++;}
  await pause(350);
  check('Current activity follows the new user message',afterFollowup());
  check('Completed answer does not acquire current Working state',!document.querySelector('[data-message="a1"] .loading-state'));
  emit('snapshot',snapshot());await pause(100);
  check('Repeated snapshot keeps one activity after the follow-up',document.querySelectorAll('.loading-state').length===1&&afterFollowup());
  document.querySelector('#project-list button').click();await pause(100);
  check('Project resubscription preserves chronological activity',afterFollowup());
  const reply={id:'a2',speaker:'manager',content:'Processing follow-up',status:'streaming'};messages.push(reply);emit('output',reply);await pause(100);
  check('New reply owns the single active footer',document.querySelectorAll('.loading-state').length===1&&!!document.querySelector('[data-message="a2"] .loading-state'));
  project.busy=false;project.pending=0;reply.status='complete';reasoning.manager.running=false;reasoning.manager.duration=2000000000;emit('snapshot',snapshot());await pause(100);
  check('Completion clears Working and keeps finished reasoning with its reply',document.querySelectorAll('.loading-state').length===0&&document.querySelector('[data-message="a2"]').textContent.includes('Thought for'));
  results.dataset.failures=failed;results.dataset.passed=passed;document.title=`${failed?'FAIL':'PASS'} Message order (${passed}/${passed+failed})`;
 });
})();
