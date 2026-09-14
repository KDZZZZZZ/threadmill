export function IconDefinitions() {
  return <svg aria-hidden="true" style={{position:"absolute",width:0,height:0,overflow:"hidden"}}><defs>
<symbol id="i-logo" viewBox="0 0 24 24"><path d="M6 8v8M12 4v16M18 8v8" strokeWidth="1.9"/></symbol>
<symbol id="i-folder" viewBox="0 0 24 24"><path d="M3 7a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v10H3z"/></symbol>
<symbol id="i-sidebar" viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16m7-12-3 4 3 4"/></symbol>
<symbol id="i-search" viewBox="0 0 24 24"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 4 4"/></symbol>
<symbol id="i-chevron" viewBox="0 0 24 24"><path d="m6 9 6 6 6-6"/></symbol>
<symbol id="i-arrow" viewBox="0 0 24 24"><path d="M12 19V5M5 12l7-7 7 7"/></symbol>
<symbol id="i-close" viewBox="0 0 24 24"><path d="m6 6 12 12M6 18 18 6"/></symbol>
<symbol id="i-sparkle" viewBox="0 0 24 24"><path d="M12 2l2.4 7.2L22 12l-7.6 2.8L12 22l-2.4-7.2L2 12l7.6-2.8z"/></symbol>
<symbol id="i-write" viewBox="0 0 24 24"><path d="M17 3a2.8 2.8 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5z"/></symbol>
<symbol id="i-run" viewBox="0 0 24 24"><path d="M4 17l6-5-6-5M12 19h8"/></symbol>
<symbol id="i-read" viewBox="0 0 24 24"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6"/></symbol>
<symbol id="i-check" viewBox="0 0 24 24"><path d="m5 12 4 4L19 6"/></symbol>
<symbol id="i-memory" viewBox="0 0 24 24"><path d="M4 6c0-4 16-4 16 0s-16 4-16 0v12c0 4 16 4 16 0V6M4 12c0 4 16 4 16 0"/></symbol>
<symbol id="i-stop" viewBox="0 0 24 24"><rect x="6" y="6" width="12" height="12" rx="2"/></symbol>
<symbol id="i-replay" viewBox="0 0 24 24"><path d="M21 12a9 9 0 1 1-3-6.7M21 3v6h-6"/></symbol>
<symbol id="i-pause" viewBox="0 0 24 24"><path d="M8 5v14M16 5v14"/></symbol>
<symbol id="i-play" viewBox="0 0 24 24"><path d="m8 5 11 7-11 7z"/></symbol>
<symbol id="i-agents" viewBox="0 0 24 24"><circle cx="12" cy="5" r="2"/><circle cx="5" cy="19" r="2"/><circle cx="19" cy="19" r="2"/><path d="M12 7v5H5v5m7-5h7v5"/></symbol>
</defs></svg>
}

export function Icon({name, className = ""}: {name: string; className?: string}) {
  return <svg className={className} aria-hidden="true"><use href={`#i-${name}`} /></svg>
}
