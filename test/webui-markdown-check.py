"""Generate an offline browser check from the shipped WebUI renderer.

Run: python3 test/webui-markdown-check.py /tmp/threadmill-markdown-check.html
Then open that file in a browser. No server or model calls are required.
"""

import pathlib
import re
import sys


source = (pathlib.Path(__file__).resolve().parents[1] / "docs/webui-demo.html").read_text()
scripts = re.findall(r"<script\b[^>]*>(.*?)</script>", source, re.S)
escape = next(line for line in scripts[-1].splitlines() if line.startswith("const esc="))
renderer = scripts[-1].split("// BEGIN CHAT MARKDOWN", 1)[1].split("// END CHAT MARKDOWN", 1)[0]
checks = r'''
let failures=0,passed=0;
const results=document.querySelector('#results');
function check(name,fn){
 const row=document.createElement('li');
 try{fn();row.textContent='PASS — '+name;passed++;}
 catch(error){row.textContent='FAIL — '+name+': '+error.message;failures++;}
 results.append(row);
}
function assert(condition,message){if(!condition)throw Error(message);}
function render(content){const node=document.createElement('div');node.innerHTML=renderMarkdown({content});return node;}
check('Headings, emphasis, lists and quotes',()=>{
 const el=render('## 标题\n\n**粗体** *斜体* ~~删除~~\n\n- 父项\n  - 子项\n\n1. 步骤\n\n> 引用');
 assert(el.querySelector('h2')?.textContent==='标题','heading');
 assert(el.querySelector('strong')&&el.querySelector('em')&&el.querySelector('del'),'emphasis');
 assert(el.querySelector('ul ul li')&&el.querySelector('ol li')&&el.querySelector('blockquote'),'blocks');
});
check('Code preserves indentation and literal HTML',()=>{
 const code='  <b>& literal</b>';
 const el=render('```html\n'+code+'\n```');
 assert(el.querySelector('pre code')?.textContent===code+'\n','code text');
 assert(!el.querySelector('b'),'HTML executed inside code');
});
check('Tables have a keyboard-accessible scrolling container',()=>{
 const el=render('| A | B |\n| --- | ---: |\n| 中文 | 1 |');
 assert(el.querySelector('.md-table[tabindex="0"] table tbody td')?.textContent==='中文','table');
 assert(el.querySelector('th[align="right"]'),'alignment');
});
check('Task list inputs are disabled',()=>{
 const el=render('- [x] Done\n- [ ] Pending');
 assert(el.querySelectorAll('input[disabled]').length===2,'disabled checkboxes');
 assert(el.querySelectorAll('input[checked]').length===1,'checked state');
});
check('Safe links preserve destination and isolate their new tab',()=>{
 const a=render('[Docs](https://example.com/docs?q=one&b=two)').querySelector('a');
 assert(a?.getAttribute('href')==='https://example.com/docs?q=one&b=two','destination');
 assert(a.target==='_blank'&&a.rel.includes('noopener')&&a.rel.includes('noreferrer'),'tab isolation');
});
check('JavaScript, encoded JavaScript and data links are inert',()=>{
 for(const href of ['javascript:alert(1)','jav&#x61;script:alert(1)','data:text/html,unsafe']){
  const el=render('[bad]('+href+')');
  assert(!el.querySelector('a[href]'),'unsafe href: '+href);
 }
});
check('Raw HTML is shown literally instead of creating elements',()=>{
 const text='<img src=x onerror="window.__markdownInjected=1"><script>window.__markdownInjected=1</script>';
 const el=render(text);
 assert(!el.querySelector('img,script'),'active HTML');
 assert(el.textContent.includes('<img')&&el.textContent.includes('<script>'),'literal source lost');
 assert(!window.__markdownInjected,'script ran');
});
check('Unsafe Markdown image source is removed',()=>{
 assert(!render('![test](javascript:alert(1))').querySelector('img[src]'),'unsafe image source');
});
check('Streaming fence and same-message cache update',()=>{
 const message={content:'```python\nprint("hello")'};
 const partial=renderMarkdown(message);
 assert(partial.includes('<pre')&&partial.includes('print('),'partial fence');
 assert(renderMarkdown(message)===partial,'unchanged message');
 message.content+='\n```\n\n**Finished**';
 const el=document.createElement('div');el.innerHTML=renderMarkdown(message);
 assert(el.querySelector('strong')?.textContent==='Finished','stale cached content');
 assert(el.querySelector('pre code')?.textContent==='print("hello")\n','closed fence');
});
check('Every prefix of a streamed message can render safely',()=>{
 const text='## Live\n\n**strong** [link](https://example.com)\n\n```js\nconst x = "<tag>";\n```';
 const message={content:''};
 for(let i=0;i<=text.length;i++){
  message.content=text.slice(0,i);
  const el=document.createElement('div');el.innerHTML=renderMarkdown(message);
  assert(!el.querySelector('script,iframe,style'),'active element at '+i);
 }
});
document.querySelector('#summary').textContent=`${passed} passed, ${failures} failed`;
document.body.dataset.failures=String(failures);
'''
javascript = "\n".join([*scripts[:-1], escape, renderer, checks]).replace("</script", "<\\/script")
page = '<!doctype html><meta charset="utf-8"><title>WebUI Markdown checks</title>'
page += '<style>body{font:16px/1.8 system-ui;margin:40px}li{margin:8px 0}</style>'
page += '<h1>WebUI Markdown checks</h1><p id="summary">Running…</p><ol id="results"></ol>'
page += '<script>' + javascript + '</script>'
output = pathlib.Path(sys.argv[1])
output.write_text(page)
print(output)
