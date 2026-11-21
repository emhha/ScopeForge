import { build } from "vite";
import { spawn } from "node:child_process";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";
import test from "node:test";

test("task session renders real SSE without a page refresh", async t => {
 const result = await build({configFile:fileURLToPath(new URL("../../vite.config.mts",import.meta.url)),logLevel:"error",define:{"process.env.NODE_ENV":JSON.stringify("production")},build:{write:false,lib:{entry:fileURLToPath(new URL("./task-live-browser.fixture.tsx",import.meta.url)),formats:["iife"],name:"TaskLiveRegression"}}});
 const outputs=(Array.isArray(result)?result:[result]).flatMap(x=>x.output);
 const js=outputs.filter(x=>x.type==="chunk").map(x=>x.code).join("\n");
 const css=outputs.filter(x=>x.type==="asset"&&x.fileName.endsWith(".css")).map(x=>x.source).join("\n");
 const dir=await mkdtemp(path.join(tmpdir(),"scopeforge-live-"));
 t.after(()=>rm(dir,{recursive:true,force:true}));
 const html=path.join(dir,"fixture.html");
 await writeFile(html,`<!doctype html><html><head><meta charset="utf-8"><style>${css}</style></head><body><div id="root"></div><script>${js.replaceAll("</script","<\\/script")}</script></body></html>`);
 const python=String.raw`
import json,os,shutil,sys,threading,queue
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
clients=[]
html=open(sys.argv[1],'rb').read()
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_GET(self):
  if '/activity/stream' in self.path:
   self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Cache-Control','no-cache');self.end_headers()
   self.wfile.write(b': ready\n\n');self.wfile.flush()
   q=queue.Queue();clients.append(q)
   try:
    while True:
     event=q.get(timeout=30)
     self.wfile.write(('id: '+str(event['seq'])+'\ndata: '+json.dumps(event)+'\n\n').encode());self.wfile.flush()
   except Exception: pass
  elif self.path.startswith('/api/'):
   self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(b'{"items":[],"running":false}')
  else:
   self.send_response(200);self.send_header('Content-Type','text/html');self.end_headers();self.wfile.write(html)
server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
threading.Thread(target=server.serve_forever,daemon=True).start()
options=Options();options.binary_location=shutil.which('chromium') or shutil.which('google-chrome')
for arg in ['--headless=new','--no-sandbox','--disable-dev-shm-usage','--window-size=1800,1100']:options.add_argument(arg)
options.set_capability('goog:loggingPrefs',{'browser':'ALL'})
driver=webdriver.Chrome(service=Service(shutil.which('chromedriver')),options=options)
wait=WebDriverWait(driver,12)
try:
 driver.get('http://127.0.0.1:'+str(server.server_port))
 wait.until(lambda d: 'prompt-seed' in d.find_element(By.TAG_NAME,'body').text)
 wait.until(lambda d: len(clients)>0)
 def emit(seq,worker,text,**kwargs):
  event={'seq':seq,'worker':worker,'kind':'text','summary':text[:200],'detail':text,'ts':'2026-11-21T16:00:00Z',**kwargs}
  key='main:0' if worker=='mainagent' else 'plan' if worker=='planner' else 'intent:3'
  driver.execute_script('window.fixture.history[arguments[0]].push(arguments[1])',key,event)
  for q in clients:q.put(event)
 emit(4,'mainagent','main-live-one',message_id='reply',revision=1,streaming=True)
 wait.until(lambda d:'main-live-one' in d.find_element(By.TAG_NAME,'body').text)
 emit(5,'mainagent','main-live-two',message_id='reply',revision=2,streaming=True)
 wait.until(lambda d:'main-live-two' in d.find_element(By.TAG_NAME,'body').text)
 driver.find_element(By.XPATH,"//button[contains(.,'规划 Planner')]").click()
 wait.until(lambda d:'planner-seed' in d.find_element(By.TAG_NAME,'body').text)
 emit(6,'planner','thinking-prefix '+('x'*240)+' planner-live-one',kind='thinking',message_id='thought',revision=1,streaming=True)
 wait.until(lambda d:'planner-live-one' in d.find_element(By.CSS_SELECTOR,'[data-testid=standard-chat-view]').text)
 emit(7,'planner','thinking-prefix '+('x'*240)+' planner-live-two',kind='thinking',message_id='thought',revision=2,streaming=True)
 wait.until(lambda d:'planner-live-two' in d.find_element(By.CSS_SELECTOR,'[data-testid=standard-chat-view]').text)
 driver.find_element(By.XPATH,"//button[contains(.,'worker-seed')]").click()
 emit(8,'work#1','worker-live',intent_id='3')
 wait.until(lambda d:'worker-live' in d.find_element(By.TAG_NAME,'body').text)
 assert len(clients)==1, len(clients)
 print(json.dumps({'received':len(driver.execute_script('return fixture.received')),'history_loads':driver.execute_script('return fixture.loads'),'refreshes':0}))
except Exception:
 print('browser logs:',driver.get_log('browser'))
 print('fixture state:',driver.execute_script('return window.fixture'))
 print('rendered text:',driver.find_element(By.TAG_NAME,'body').text)
 raise
finally:
 driver.quit();server.shutdown()
`;
 const noProxy=[process.env.NO_PROXY,"localhost","127.0.0.1"].filter(Boolean).join(",");
 const output=await new Promise(resolve=>{const child=spawn("python3",["-c",python,html],{env:{...process.env,NO_PROXY:noProxy,no_proxy:noProxy}});let stdout="",stderr="";child.stdout.on("data",x=>stdout+=x);child.stderr.on("data",x=>stderr+=x);child.on("error",e=>resolve({code:-1,stdout,stderr:String(e)}));child.on("close",code=>resolve({code,stdout,stderr}));});
 assert.equal(output.code,0,output.stdout+"\n"+output.stderr);
 console.log(output.stdout.trim());
});
