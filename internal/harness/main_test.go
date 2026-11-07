package harness

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestMainUsageSupport(t *testing.T) {
	path := filepath.Join("testdata", "transcript-claude", "s1.jsonl")
	for _, h := range []task.Harness{"", task.Claude, task.Pi, task.Omp, task.Codex} {
		u, supported, err := MainUsage(h, path, time.Time{}, time.Time{})
		want := h == "" || h == task.Claude
		if err != nil || supported != want || (u != nil) != want {
			t.Fatalf("%q: usage=%+v, supported=%v, err=%v", h, u, supported, err)
		}
		if want && u.Calls != 4 {
			t.Errorf("%q: calls=%d", h, u.Calls)
		}
	}
	if _, _, err := MainUsage("unknown", path, time.Time{}, time.Time{}); err == nil {
		t.Fatal("unknown harness accepted")
	}
	u, supported, err := MainUsage("", filepath.Join(t.TempDir(), "missing"), time.Time{}, time.Time{})
	if !supported || u != nil || err == nil {
		t.Fatalf("missing transcript confused with unsupported: %+v, %v, %v", u, supported, err)
	}
}

func TestMainExtensionFiles(t *testing.T) {
	for _, h := range []task.Harness{task.Pi, task.Omp} {
		file, err := MainExtension("/opt/kit quoted/\"hrk\\binary", h)
		if err != nil || file.Name != "hrk-main.ts" {
			t.Fatalf("%s: %v %+v", h, err, file)
		}
		checkScript(t, string(file.Data))
		if strings.Contains(string(file.Data), "__HRK_") {
			t.Fatal("unrendered placeholder")
		}
	}
	for _, tc := range []struct {
		bin string
		h   task.Harness
	}{{"relative", task.Pi}, {"/bin/hrk", task.Claude}, {"/bin/hrk", task.Codex}} {
		if _, err := MainExtension(tc.bin, tc.h); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestMainExtensionLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to verify the main extension")
	}
	for _, h := range []task.Harness{task.Pi, task.Omp} {
		t.Run(string(h), func(t *testing.T) {
			dir := t.TempDir()
			file, err := MainExtension("/opt/hrk", h)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "main.mjs"), string(file.Data))
			writeFile(t, filepath.Join(dir, "driver.mjs"), mainLifecycleDriver)
			cmd := exec.Command(node, filepath.Join(dir, "driver.mjs"), string(h))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("node lifecycle: %v\n%s", err, out)
			} else {
				t.Log(string(out))
			}
		})
	}
}

const mainLifecycleDriver = `import assert from 'node:assert/strict';
import cp from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
import {EventEmitter} from 'node:events';
import {env,pid} from 'node:process';
const harness=process.argv[2];
const beforeBranch=harness==='omp'?'session_before_branch':'session_before_fork';
let now=100000, serial=0, timers=new Map();
Date.now=()=>now;
globalThis.setTimeout=(fn,ms=0)=>{ const id=++serial;timers.set(id,{fn,at:now+ms});return id; };
globalThis.clearTimeout=id=>timers.delete(id);
async function advance(ms=0) {
  const end=now+ms;
  for(let count=0;count<1000;count++) {
    await Promise.resolve();await Promise.resolve();
    const next=[...timers].filter(([,v])=>v.at<=end).sort((a,b)=>a[1].at-b[1].at)[0];
    if(!next){now=end;await Promise.resolve();return;}
    now=next[1].at;timers.delete(next[0]);next[1].fn();
  }
  throw Error('timer hot loop');
}
let generation=0, children=[], requests=[], disk=false, saved=null, closeRace=false, stubborn=false;
class Child extends EventEmitter {
 constructor(){super();this.exitCode=null;this.signalCode=null;this.stdout=new EventEmitter();this.stdout.setEncoding=()=>{};
 this.stdin=new EventEmitter();this.stdin.writable=true;this.stdin.end=()=>{};
 this.stdin.write=line=>{const q=JSON.parse(line);requests.push(q);setTimeout(()=>this.handle(q),0);return true;};}
 reply(q,type,more={}){this.stdout.emit('data',JSON.stringify({version:1,type,requestId:q.requestId,generation:this.generation,...more})+'\n');}
 handle(q){if(this.exitCode!==null||this.signalCode!==null)return;
  if(q.type==='register') {this.register=q;this.generation=++generation;this.reply(q,'ready');
   if(saved) this.reply(q,disk?'committed':'conflict',{batchId:saved.batchId,message:'prior append may still finish; verify its receipt or exit the main process and explicitly resume'});
  } else if(q.type==='receipt') {this.reply(q,disk?'committed':'error',{batchId:q.batchId,message:'receipt not on disk; retain pending and do not resend'});if(disk)saved=null;}
  else if(q.type==='activity')this.reply(q,'ready');
  else if(q.type==='close') {if(closeRace){closeRace=false;deliver(this);this.reply(q,'conflict',{message:'pending batch'});}
   else this.reply(q,'ready',{message:'closed'});}
 }
 kill(signal){if(stubborn&&signal!=='SIGKILL')return;this.signalCode=signal;setTimeout(()=>{this.emit('exit');this.emit('close');},0);}
}
cp.spawn=(bin,args,options)=>{assert.equal(bin,'/opt/hrk');assert.deepEqual(args,['main','__news']);assert.equal(options.detached,undefined);const c=new Child();children.push(c);return c;};
const raw=new EventEmitter();raw.isTTY=true;raw.isRaw=false;raw.readableFlowing=false;
const runtimeInput=()=>{};const observers=()=>raw.listeners('data').filter(f=>f!==runtimeInput).length;
Object.defineProperty(process,'stdin',{value:raw});
syncBuiltinESMExports();
const {default:extension}=await import('./main.mjs');
env.HRK_MAIN=String(pid);env.HRK_MAIN_OWNER='/owner';env.HRK_MAIN_NONCE='fixture-nonce';env.HRK_MAIN_HARNESS=harness;env.HRK_MAIN_INTERACTIVE='1';delete env.HRK_TASK;
let text='',idle=true,queued=false,focus,overlay=false,listeners=new Set(),notices=[],statuses=[],sent=[];
class CustomEditor {getText(){return text;} onSubmit(){} }
const editor=new CustomEditor();focus=editor;
const tui={getFocusedComponent:harness==='pi'?()=>focus:undefined,getFocused:harness==='omp'?()=>focus:undefined,hasOverlay:()=>overlay,children:[editor],setFocus(value){focus=value;return 'focus-return';}};
const originalFocus=tui.setFocus;
function setFocus(value){assert.equal(tui.setFocus(value),'focus-return');}
let session={sessionId:'one',sessionFile:'/sessions/one.jsonl',cwd:'/cwd'};
const ctx={mode:'tui',hasUI:true,model:{},isIdle:()=>idle,hasPendingMessages:()=>queued,
 sessionManager:{getSessionId:()=>session.sessionId,getSessionFile:()=>session.sessionFile,getCwd:()=>session.cwd},
 ui:{getEditorText:()=>text,onTerminalInput:fn=>{listeners.add(fn);return()=>listeners.delete(fn);},
 setWidget:(_k,fn)=>{if(fn)fn(tui);},setStatus:(_k,v)=>statuses.push(v),notify:v=>notices.push(v)}};
function make(){const h={};extension({on:(name,f)=>h[name]=f,sendMessage:(m,o)=>{assert.equal(idle,true);assert.equal(text,'');assert.equal(queued,false);assert.equal(focus,editor);assert.equal(overlay,false);assert.deepEqual(o,{deliverAs:'followUp',triggerTurn:true});sent.push(m);}});return h;}
function rawInput(data){const original=Buffer.isBuffer(data)?Buffer.from(data):data;raw.emit('data',data);assert.deepEqual(data,original,'raw input changed');}
function key(data){rawInput(data);for(const f of listeners)assert.equal(f(data),undefined,'input consumed');}
async function start(h,proveInput=false){if(!raw.listeners('data').includes(runtimeInput))raw.on('data',runtimeInput);raw.readableFlowing=true;raw.isRaw=true;h.session_start({},ctx);if(proveInput)rawInput('\x1b[201~');await advance(600);}
async function stop(h){const promise=h.session_shutdown({reason:'reload'},ctx);await advance(1800);await promise;assert.equal(children.filter(c=>c.signalCode===null).length,0);assert.equal(listeners.size,0);assert.equal(observers(),0);assert.equal(raw.listenerCount('newListener'),0);assert.equal(raw.listenerCount('removeListener'),0);assert.equal(tui.setFocus,originalFocus);}
let batchSeq=0;
function deliver(c=children.at(-1)){disk=false;saved={batchId:'b'+(++batchSeq),digest:'digest',lines:['t1 done','second line'],target:{...session,owner:'/owner',harness},attempt:'appending'};c.reply(c.register,'batch',{batchId:saved.batchId,batch:saved});return saved;}
function end(h){idle=true;if(harness==='pi')h.agent_settled({},ctx);else h.agent_end({willContinue:false},ctx);}
// Guards must not register hooks or spawn outside a launched main TUI.
env.HRK_TASK='runner';assert.deepEqual(Object.keys(make()),[]);delete env.HRK_TASK;
env.HRK_MAIN_INTERACTIVE='0';assert.deepEqual(Object.keys(make()),[]);env.HRK_MAIN_INTERACTIVE='1';
let h=make();assert.equal(raw.listenerCount('data'),0,'observer started stdin before the runtime');ctx.mode='rpc';h.session_start({},ctx);await advance(1000);assert.equal(children.length,0);ctx.mode='tui';
await start(h);assert.equal(children.length,1);assert.equal(sent.length,0);assert.equal(requests[0].type,'register');assert.equal(requests[0].cwd,'/cwd');
// An idle fresh main needs no input or focus transition to receive news.
deliver();await advance(600);assert.equal(sent.length,1,'fresh idle main did not deliver');
disk=true;h.message_end({},ctx);await advance(600);
// Raw chunks are hidden from the runtime listener/editor until paste END.
rawInput('\x1b[200~');deliver();await advance(1500);assert.equal(sent.length,1,'injected inside raw paste');
const beforeWarning=notices.length;
rawInput('held');await advance(9999);assert.equal(notices.length,beforeWarning,'warned before 10s without bytes');
await advance(1);assert.ok(statuses.at(-1)?.includes('Paste incomplete'),'missing paste warning');
await advance(20000);assert.equal(sent.length,1,'timeout opened paste guard');
// Omp may emit a synthetic completed paste after its timeout; raw END is still required.
for(const f of listeners)f('\x1b[200~held\x1b[201~');await advance(600);assert.equal(sent.length,1);
// Reload cannot reopen an unfinished paste and disposes the old raw listener.
await stop(h);h=make();await start(h);assert.equal(observers(),1);assert.equal(sent.length,1);
rawInput('\x1b[20');await advance(600);assert.equal(sent.length,1);
rawInput('1~');await advance(600);assert.equal(sent.length,2,'real END did not release paste');
disk=true;h.message_end({},ctx);await advance(600);
// Split START, multiple markers in one chunk, Buffer input and delayed prefixes.
rawInput(Buffer.from('\x1b[20'));deliver();await advance(600);assert.equal(sent.length,2);
rawInput('0~first\x1b[201~\x1b[200~second');await advance(600);assert.equal(sent.length,2);
rawInput('\x1b[201~');await advance(600);assert.equal(sent.length,3);
disk=true;h.message_end({},ctx);await advance(600);
// An unobserved reload gap cannot assume the parser is idle, even with an empty editor.
await stop(h);rawInput('unobserved');h=make();await start(h);deliver();await advance(12000);assert.equal(sent.length,3);
key('a');await advance(600);assert.equal(sent.length,3,'parsed key cleared unknown paste state');
rawInput('\x1b[201~');await advance(600);assert.equal(sent.length,4);
disk=true;h.message_end({},ctx);await advance(600);
// Ordinary Escape is a partial marker until the runtime confirms it was a key.
key('\x1b');deliver();await advance(600);assert.equal(sent.length,5,'Escape wedged delivery');
disk=true;h.message_end({},ctx);await advance(600);sent.length=0;batchSeq=0;
const activityCount=()=>requests.filter(q=>q.type==='activity').length;
const quiet=activityCount();await advance(10000);assert.equal(activityCount(),quiet,'technical time marked activity');
rawInput('\x1b[?1;2c');await advance(600);assert.equal(activityCount(),quiet,'terminal reply marked user activity');
const connections=children.length;h.session_start({},ctx);await advance();assert.equal(children.length,connections);assert.equal(listeners.size,1);
// Exact whitespace, chunked paste, debounce, queues, busy/retry and modal gates.
text=' \n\t';deliver();await advance(1500);assert.equal(sent.length,0);assert.equal(text,' \n\t');
assert.deepEqual(await h[beforeBranch]({},ctx),{cancel:true},'unsent news allowed a branch');
assert.equal(children.at(-1).register.sessionId,'one');
key('\x1b[200~');text='draft\n';key('chunk');await advance(900);assert.equal(sent.length,0);
text='';key('\x1b[201~');await advance(499);assert.equal(sent.length,0);queued=true;await advance(600);assert.equal(sent.length,0);
queued=false;h.input({source:'interactive',text:'user prompt'},ctx);await advance(600);assert.equal(sent.length,0);idle=false;h.agent_start({},ctx);await advance(600);assert.equal(sent.length,0);
if(harness==='omp'){h.session_stop({},ctx);h.agent_end({willContinue:true},ctx);await advance(600);assert.equal(sent.length,0);}
// A queued prompt can drain within the existing run, without another agent_start.
queued=true;h.input({source:'interactive',text:'queued prompt'},ctx);text='draft during busy';
h.message_end({},ctx);await advance(600);assert.equal(sent.length,0);
h.auto_retry_start({},ctx);end(h);await advance(600);assert.equal(sent.length,0);h.auto_retry_end({},ctx);
await advance(600);assert.equal(sent.length,0,'settled bypassed queued input');
queued=false;await advance(600);assert.equal(sent.length,0);assert.equal(text,'draft during busy');
text='';key('clear');await advance(499);assert.equal(sent.length,0,'settled bypassed debounce');
h.auto_retry_start({},ctx);end(h);await advance(600);assert.equal(sent.length,0);h.auto_retry_end({},ctx);
setFocus({});await advance(600);assert.equal(sent.length,0);setFocus(editor);overlay=true;await advance(600);assert.equal(sent.length,0);overlay=false;
h.ui_prompt_start({},ctx);await advance(600);assert.equal(sent.length,0);h.ui_prompt_end({},ctx);
const abort=new AbortController();h.session_before_compact({signal:abort.signal},ctx);await advance(600);assert.equal(sent.length,0);abort.abort();
if(harness==='omp') {
 await advance(600);assert.equal(sent.length,0,'abort signal released unfinished manual compaction');
 tui.children.push({getText:()=> 'Error: Compaction cancelled'});
}
h.auto_compaction_start({},ctx);await advance(600);assert.equal(sent.length,0);h.auto_compaction_end({},ctx);await advance(600);assert.equal(sent.length,1);
assert.equal(sent[0].content,'t1 done\nsecond line');assert.deepEqual(sent[0].details,{batchId:'b1',owner:'/owner',digest:'digest'});
// Receipt retries and duplicate lifecycle events never resend an attempted batch.
for(let i=0;i<4;i++){h.message_end({},ctx);end(h);await advance(2000);}
assert.equal(sent.length,1);assert.deepEqual(await h.session_before_switch({},ctx),{cancel:true});
assert.deepEqual(await h[beforeBranch]({},ctx),{cancel:true},'uncertain receipt allowed a branch');
disk=true;h.message_end({},ctx);await advance(600);assert.equal(sent.length,1);
// Never-sent handover survives teardown and reconnect, but old callbacks cannot drain.
text='keep this exact draft';deliver();await advance(600);const old=h;await stop(h);h=make();await start(h,true);
assert.equal(sent.length,1);assert.equal(text,'keep this exact draft');assert.equal(listeners.size,1);
old.agent_settled({},ctx);old.message_end({},ctx);await advance(600);assert.equal(sent.length,1);
text='';key('clear');await advance(600);assert.equal(sent.length,2);
// Inflight without receipt remains receipt-only across repeated reloads.
await stop(h);h=make();await start(h,true);await advance(60000);assert.equal(sent.length,2);
await stop(h);h=make();await start(h,true);assert.equal(sent.length,2);
disk=true;h.message_end({},ctx);await advance(600);assert.equal(sent.length,2);
// Receipt persisted before reload is committed without another send.
deliver();await advance(600);assert.equal(sent.length,3);disk=true;await stop(h);saved=null;h=make();await start(h,true);assert.equal(sent.length,3);
// Child death reconnects on the same nonce and cannot replay an uncertain append.
deliver();await advance(600);assert.equal(sent.length,4);children.at(-1).kill('SIGKILL');await advance(4000);assert.equal(sent.length,4);
disk=true;h.message_end({},ctx);await advance(600);
// Close racing a new batch cancels transition, preserving its original target.
closeRace=true;const transition=h[beforeBranch]({},ctx);await advance(2000);assert.deepEqual(await transition,{cancel:true});
await advance(4000);assert.equal(sent.length,5);disk=true;h.message_end({},ctx);await advance(600);
// Only Pi includes tree summarization in idle; Omp needs explicit recovery.
for (const aborted of [true,false]) {
 idle=harness==='omp';const signal=new AbortController();const navigation=h.session_before_tree({signal:signal.signal},ctx);
 await advance(2000);assert.equal(await navigation,undefined);
 const count=children.length;await advance(1000);assert.equal(children.length,count,'reconnected during navigation');
 if(aborted)signal.abort();idle=true;await advance(1000);
 if(harness==='omp') {
  idle=false;await advance(600);idle=true;await advance(600);
  assert.equal(children.length,count,'unrelated busy/idle reopened an unknown tree operation');
  h.input({source:'interactive',text:'/reload-plugins'},ctx);await advance(1000);
 }
 assert.equal(children.length,count+1,'cancelled/failed tree left delivery suspended');
 assert.equal(children.at(-1).register.sessionId,'one');
}
// Later hooks may cancel switch/fork without a completion event. Show explicit recovery.
for(const name of ['session_before_switch',beforeBranch]) {
 const cancelled=h[name]({},ctx);await advance(2000);assert.equal(await cancelled,undefined);
 assert.ok(statuses.some(s=>s?.includes('/reload to reconnect')));
 const previous=children.length;await advance(2000);assert.equal(children.length,previous);
 h.input({source:'interactive',text:harness==='omp'&&name===beforeBranch?'/reload-plugins':'/reload'},ctx);await advance(1000);
 assert.equal(children.length,previous+1);assert.equal(children.at(-1).register.sessionId,'one');
}
// A successful quiesced close permits new identity and repeated session_start.
const change=h.session_before_switch({},ctx);await advance(2000);assert.equal(await change,undefined);
session={sessionId:'two',sessionFile:'/sessions/two.jsonl',cwd:'/cwd'};
if(harness==='omp'){h.session_switch({reason:'new'},ctx);await advance(600);}else await start(h);
assert.equal(children.at(-1).register.sessionId,'two');assert.equal(sent.length,5);
const switched=children.length;h.session_start({},ctx);await advance(600);assert.equal(children.length,switched);
if(harness==='omp'){h.session_switch({reason:'new'},ctx);await advance(600);assert.equal(children.length,switched);}
assert.equal(children.filter(c=>c.signalCode===null).length,1);assert.equal(listeners.size,1);
// Unknown UI APIs fail closed, with pending visible and no input changes.
const savedOverlay=tui.hasOverlay;tui.hasOverlay=undefined;deliver();await advance(1000);assert.equal(sent.length,5);tui.hasOverlay=savedOverlay;
h.session_before_compact({},ctx);await advance(600);assert.equal(sent.length,5);h.session_compact_failed({aborted:true},ctx);await advance(600);assert.equal(sent.length,6);
disk=true;h.message_end({},ctx);await advance(600);
// Omp is idle even during manual compaction and only presents a TUI error on failure.
const failure=()=>({getText:()=> 'Error: Compaction failed: HTTP 400'});
const staleFailure=failure();tui.children.push(staleFailure);
h.session_before_compact({},ctx);deliver();await advance(60000);assert.equal(sent.length,6,'stale error or idle released running compaction');
tui.children.push({getText:()=> 'Error: Compaction failed: Compaction already in progress'});
await advance(600);assert.equal(sent.length,6,'rejected overlapping compaction released running compaction');
tui.children.push({getText:()=> 'Error: Unrelated failure'});
await advance(600);assert.equal(sent.length,6,'unrelated error released running compaction');
idle=false;tui.children.push(failure());await advance(600);assert.equal(sent.length,6,'compaction failure bypassed busy runtime');
idle=true;await advance(600);
if(harness==='pi') {
 assert.equal(sent.length,6,'Omp error fallback affected Pi');h.session_compact_failed({},ctx);await advance(600);
}
assert.equal(sent.length,7,'manual provider failure left news pending');
await advance(2000);assert.equal(sent.length,7,'failure resent news');
disk=true;h.message_end({},ctx);await advance(600);
if(harness==='omp') {
 // An extension veto throws CompactionCancelledError without aborting the signal.
 const before=sent.length, signal=new AbortController();
 const cancelled=()=>({getText:()=> 'Error: Compaction cancelled'});
 tui.children.push(cancelled());h.session_before_compact({signal:signal.signal},ctx);deliver();
 await advance(600);assert.equal(sent.length,before,'stale cancellation released compaction');
 tui.children.push(cancelled());await advance(600);
 assert.equal(signal.signal.aborted,false);assert.equal(sent.length,before+1,'extension veto left compaction blocked');
 disk=true;h.message_end({},ctx);await advance(600);
}
// Omp's threshold maintenance follows agent_end(willContinue), with no later agent_end.
for(const outcome of ['success','cancel','failure']) {
 const before=sent.length, signal=new AbortController();
 idle=false;h.agent_start({},ctx);
 if(harness==='omp') {
  h.auto_compaction_start({reason:'threshold'},ctx);idle=true;h.agent_end({willContinue:true},ctx);
 } else h.agent_end({},ctx);
 h.session_before_compact({signal:signal.signal,reason:'threshold'},ctx);deliver();
 await advance(1000);assert.equal(sent.length,before,'delivered during automatic compaction');
 if(outcome==='success')h.session_compact({},ctx);
 if(outcome==='cancel')signal.abort();
 if(harness==='pi'&&outcome!=='success')h.session_compact_failed({aborted:outcome==='cancel'},ctx);
 await advance(1000);assert.equal(sent.length,before,'released before automatic maintenance finished');
 if(harness==='omp')h.auto_compaction_end({aborted:outcome==='cancel'},ctx);
 if(harness==='pi')end(h);
 await advance(600);assert.equal(sent.length,before+1,'automatic compaction left idle news blocked: '+outcome);
 disk=true;h.message_end({},ctx);await advance(600);
}
// Overflow/retry or a queued continuation can outlive maintenance's end event.
for(const kind of ['continuation','retry','queued']) {
 const before=sent.length;
 idle=false;h.agent_start({},ctx);deliver();
 if(harness==='omp')h.agent_end({willContinue:true},ctx);else h.agent_end({},ctx);
 if(kind==='retry')h.auto_retry_start({},ctx);
 if(kind==='queued')queued=true;
 h.session_before_compact({reason:'overflow'},ctx);h.session_compact({},ctx);
 if(harness==='omp')h.auto_compaction_end({aborted:false,willRetry:true},ctx);
 await advance(600);assert.equal(sent.length,before,'maintenance end bypassed '+kind);
 if(kind==='retry')h.auto_retry_end({success:true},ctx);
 if(kind==='queued')queued=false;
 end(h);await advance(600);assert.equal(sent.length,before+1,'continuation did not settle');
 disk=true;h.message_end({},ctx);await advance(600);
}
// A handled input has no terminal event. Warn locally without guessing it settled.
const recoveryWarning=s=>s?.startsWith('News still paused.');
for(const blocked of ['idle','busy','draft','queue']) {
 const before=sent.length;
 h.input({source:'interactive',text:'handled by another extension'},ctx);
 let offset=statuses.length;await advance(31000);
 assert.equal(statuses.slice(offset).some(recoveryWarning),false,'warned without pending news');
 if(blocked==='busy')idle=false;
 if(blocked==='draft')text=' ';
 if(blocked==='queue')queued=true;
 deliver();await advance(9999);
 assert.equal(statuses.slice(offset).some(recoveryWarning),false,'warned before pending threshold');
 const activity=activityCount();await advance(1);
 assert.equal(statuses.slice(offset).some(recoveryWarning),blocked==='idle','warning ignored runtime/editor/queue state');
 assert.equal(sent.length,before,'warning delivered news');
 idle=true;text='';queued=false;await advance(500);
 assert.ok(recoveryWarning(statuses.at(-1)));
 assert.ok(statuses.at(-1).includes(harness==='omp'?'/reload-plugins':'/reload'));
 assert.ok(statuses.at(-1).includes('exit and explicitly resume'));
 await advance(30000);assert.equal(sent.length,before,'warning timeout opened gate');
 assert.equal(activityCount(),activity,'warning marked technical activity as user activity');
 await stop(h);h=make();await start(h,true);
 assert.equal(sent.length,before+1,'explicit reload did not recover the never-sent batch');
 disk=true;h.message_end({},ctx);await advance(600);
}
// Extension-owned compaction with no completion stays closed even when warning.
if(harness==='omp') {
 const before=sent.length;
 h.session_before_compact({},ctx);deliver();await advance(10000);
 assert.ok(recoveryWarning(statuses.at(-1)));assert.equal(sent.length,before);
 h.input({source:'interactive',text:'/reload-plugins'},ctx);await advance(600);
 assert.equal(sent.length,before,'same-instance reload cleared unknown compaction');
 h.session_compact({},ctx);await advance(600);
 assert.equal(sent.length,before+1);disk=true;h.message_end({},ctx);await advance(600);
}
// Omp branches emit only before_branch then session_branch, with no switch/start.
if(harness==='omp') {
 const previous=children.at(-1), count=children.length, before=sent.length;
 const branch=h.session_before_branch({},ctx);await advance(2000);assert.equal(await branch,undefined);
 assert.notEqual(previous.signalCode,null);assert.equal(children.length,count);
 session={sessionId:'branched',sessionFile:'/sessions/branched.jsonl',cwd:'/cwd'};
 text='restored user entry';h.session_branch({},ctx);await advance(600);
 assert.equal(children.length,count+1);assert.equal(children.at(-1).register.sessionId,'branched');
 assert.equal(children.at(-1).register.sessionFile,'/sessions/branched.jsonl');
 assert.equal(children.at(-1).generation,previous.generation+1);
 h.session_branch({},ctx);await advance(600);assert.equal(children.length,count+1);
 assert.equal(children.filter(c=>c.signalCode===null).length,1);assert.equal(listeners.size,1);
 deliver();await advance(600);assert.equal(sent.length,before);assert.equal(text,'restored user entry');
 text='';key('clear');await advance(600);assert.equal(sent.length,before+1);
 disk=true;h.message_end({},ctx);await advance(600);assert.equal(saved,null);
}
// A stuck child is force-stopped within the shutdown budget.
stubborn=true;const began=now;await stop(h);assert.ok(now-began<=1800);assert.equal(timers.size,0);
// Some hosts reuse the extension instance after a shutdown/session start.
await start(h);assert.equal(observers(),1);await stop(h);assert.equal(timers.size,0);
console.log('main lifecycle passed: guards, drafts/paste, debounce, queues, retry/compaction, modal, receipts, reload, stale callbacks, child death, transition race, bounded cleanup');
`

func TestMainSpecAndArgs(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"claude", []string{"claude", "--resume", "session"}},
		{"claude:opus:high", []string{"claude", "--model", "opus", "--effort", "high", "--resume", "session"}},
		{"pi", []string{"pi", "-e", "/extension", "--append-system-prompt", "/prompt", "--session", "session"}},
		{"omp", []string{"omp", "--hook", "/extension", "--append-system-prompt", "/prompt", "--resume", "session"}},
		{"pi:ollama/qwen3:8b:high", []string{"pi", "-e", "/extension", "--append-system-prompt", "/prompt", "--model", "ollama/qwen3:8b", "--thinking", "high", "--session", "session"}},
		{"omp:provider/model:auto", []string{"omp", "--hook", "/extension", "--append-system-prompt", "/prompt", "--model", "provider/model", "--thinking", "auto", "--resume", "session"}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			spec, err := ParseMainSpec(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := MainArgs(spec, "/extension", "/prompt", "session"); !slices.Equal(got, tc.want) {
				t.Fatalf("%q, want %q", got, tc.want)
			}
		})
	}
	for _, input := range []string{"codex", "codex:model:high", "pi::high", "omp::auto", "pi:model:auto", "unknown", "claude:model:off"} {
		if _, err := ParseMainSpec(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	for _, input := range []string{"pi", "omp"} {
		if _, err := task.ParseAgentSpec(input); err == nil {
			t.Errorf("runner parser accepted bare %s", input)
		}
	}
}
