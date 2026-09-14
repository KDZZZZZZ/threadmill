import { Marked } from 'marked'
import DOMPurify from 'dompurify'

const esc = (s: string) => s.replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]!))
const markdownParser=new Marked({gfm:true,breaks:true,renderer:{html:({text})=>esc(text)}});
const markdownCache=new WeakMap<object, {source: string; html: string}>();
export function renderMarkdown(message: {content: string}){
 const source=String(message.content??''),cached=markdownCache.get(message);
 if(cached?.source===source)return cached.html;
 const fragment=DOMPurify.sanitize(markdownParser.parse(source, {async: false}),{
  RETURN_DOM_FRAGMENT:true,ALLOW_DATA_ATTR:false,ALLOW_ARIA_ATTR:false,
  ALLOWED_TAGS:['p','br','hr','h1','h2','h3','h4','h5','h6','strong','em','del','blockquote','ul','ol','li','pre','code','a','img','table','thead','tbody','tr','th','td','input'],
  ALLOWED_ATTR:['href','title','src','alt','class','align','start','type','checked','disabled']
 });
 for(const a of fragment.querySelectorAll<HTMLAnchorElement>('a[href]')){a.target='_blank';a.rel='noopener noreferrer';}
 for(const img of fragment.querySelectorAll('img')){img.loading='lazy';img.referrerPolicy='no-referrer';}
 for(const table of fragment.querySelectorAll('table')){const wrap=document.createElement('div');wrap.className='md-table';wrap.tabIndex=0;wrap.setAttribute('aria-label','Table');table.replaceWith(wrap);wrap.append(table);}
 for(const pre of fragment.querySelectorAll('pre')){pre.tabIndex=0;pre.setAttribute('aria-label','Code block');}
 const container=document.createElement('div');container.append(fragment);
 const html=container.innerHTML;markdownCache.set(message,{source,html});return html;
}
