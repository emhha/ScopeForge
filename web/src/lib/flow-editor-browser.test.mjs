import { build } from "vite";
import { spawn } from "node:child_process";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";
import test from "node:test";

test("workflow canvas renders added fan-out wires, deletes them, and edits direct commands", {
  timeout: 90000,
}, async (t) => {
  const result = await build({
    configFile: fileURLToPath(new URL("../../vite.config.mts", import.meta.url)),
    logLevel: "error",
    define: { "process.env.NODE_ENV": JSON.stringify("production") },
    build: {
      write: false,
      lib: {
        entry: fileURLToPath(new URL("./flow-editor-browser.fixture.tsx", import.meta.url)),
        formats: ["iife"],
        name: "FlowEditorRegression",
      },
    },
  });
  const outputs = (Array.isArray(result) ? result : [result]).flatMap((item) => item.output);
  const js = outputs
    .filter((item) => item.type === "chunk")
    .map((item) => item.code)
    .join("\n");
  const css = outputs
    .filter((item) => item.type === "asset" && item.fileName.endsWith(".css"))
    .map((item) => item.source)
    .join("\n");
  const dir = await mkdtemp(path.join(tmpdir(), "scopeforge-flow-editor-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const html = path.join(dir, "fixture.html");
  await writeFile(
    html,
    `<!doctype html><html><head><meta charset="utf-8"><style>${css}</style></head><body><div id="root"></div><script>${js.replaceAll("</script", "<\\/script")}</script></body></html>`,
  );
  const python = String.raw`
import json,shutil,sys,threading
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.common.action_chains import ActionChains
from selenium.webdriver.common.keys import Keys
from selenium.webdriver.support.ui import WebDriverWait,Select
html=open(sys.argv[1],'rb').read()
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_GET(self):
  self.send_response(200);self.send_header('Content-Type','text/html');self.end_headers();self.wfile.write(html)
server=ThreadingHTTPServer(('127.0.0.1',0),Handler)
threading.Thread(target=server.serve_forever,daemon=True).start()
options=Options();options.binary_location=shutil.which('chromium') or shutil.which('google-chrome')
for arg in ['--headless=new','--no-sandbox','--disable-dev-shm-usage','--window-size=2200,1500']: options.add_argument(arg)
options.set_capability('goog:loggingPrefs',{'browser':'ALL'})
driver=webdriver.Chrome(service=Service(shutil.which('chromedriver')),options=options)
wait=WebDriverWait(driver,15)
def node(id): return driver.find_element(By.CSS_SELECTOR,'.react-flow__node[data-id="'+id+'"]')
def wires(): return driver.find_elements(By.CSS_SELECTOR,'.react-flow__edge-path')
def button(text): return driver.find_element(By.XPATH,'//button[normalize-space(.)="'+text+'"]')
def check():
 driver.execute_script('window.fixture.checked=null')
 button('校验').click()
 return wait.until(lambda d:d.execute_script('return window.fixture.checked'))
def pick(id): node(id).click()
def drag(source,target):
 a=node(source).find_element(By.CSS_SELECTOR,'.source')
 b=node(target).find_element(By.CSS_SELECTOR,'.target')
 ActionChains(driver).move_to_element(a).click_and_hold().pause(.15).move_to_element(b).pause(.15).release().perform()
try:
 driver.get('http://127.0.0.1:'+str(server.server_port))
 wait.until(lambda d:len(wires())==4)
 # This is the original bug: add a wire from an initially empty parallel node.
 drag('fork','dns');wait.until(lambda d:len(wires())==5)
 drag('fork','http');wait.until(lambda d:len(wires())==6)
 for edge in wires():
  assert edge.get_attribute('d').startswith('M'),edge.get_attribute('outerHTML')
  assert edge.is_displayed()
 assert driver.execute_script('return getComputedStyle(arguments[0]).stroke',wires()[0]) not in ['none','rgba(0, 0, 0, 0)']
 checked=check();assert next(n for n in checked['nodes'] if n['id']=='fork')['branches']==['dns','http']
 pick('dns')
 geometry=driver.execute_script('''const canvas=document.querySelector('[data-testid="flow-canvas"]');
 const row=canvas.parentElement, sidebar=canvas.nextElementSibling;
 return {canvas:canvas.getBoundingClientRect().height,row:row.getBoundingClientRect().height,sidebar:sidebar.getBoundingClientRect().height,overflow:getComputedStyle(sidebar).overflowY};''')
 assert abs(geometry['row']-geometry['canvas'])<2,geometry
 assert geometry['overflow']=='auto',geometry
 # Delete a selected canvas wire using the keyboard, keeping the other branch.
 edge=driver.find_element(By.CSS_SELECTOR,'.react-flow__edge[data-id="fork:branches:dns"]')
 driver.execute_script('arguments[0].dispatchEvent(new MouseEvent("click",{bubbles:true}))',edge)
 ActionChains(driver).send_keys(Keys.DELETE).perform()
 wait.until(lambda d:len(wires())==5)
 assert next(n for n in check()['nodes'] if n['id']=='fork')['branches']==['http']
 # Add it back via the accessible sidebar instead of dragging.
 pick('fork')
 Select(driver.find_element(By.CSS_SELECTOR,'[aria-label="连接目标"]')).select_by_value('dns')
 button('连接').click();wait.until(lambda d:len(wires())==6)
 pick('dns')
 program=driver.find_element(By.CSS_SELECTOR,'[aria-label="程序"]')
 program.send_keys(Keys.CONTROL,'a');program.send_keys('cat')
 Select(driver.find_element(By.CSS_SELECTOR,'[aria-label="节点工具"]')).select_by_value('Bash')
 command=driver.find_element(By.CSS_SELECTOR,'[aria-label="Shell 命令"]')
 command.send_keys(Keys.CONTROL,'a');command.send_keys("printf 'hello' | cat")
 checked=check();dns=next(n for n in checked['nodes'] if n['id']=='dns')
 assert dns['tool']=='Bash' and dns['arguments']['command']=="printf 'hello' | cat",dns
 assert not driver.find_elements(By.CSS_SELECTOR,'[aria-label="命令执行方式"]')
 assert not driver.find_elements(By.XPATH,'//button[normalize-space(.)="命令 / Bash"]')
 button('工具调用').click()
 checked=check();added=checked['nodes'][-1]
 assert added['type']=='tool' and added['tool']=='Bash',added
 button('同域名并行模板').click()
 wait.until(lambda d:next(n for n in check()['nodes'] if n['id']=='dns')['tool']=='workflow_command')
 checked=check()
 for id in ['dns','http']:
  assert next(n for n in checked['nodes'] if n['id']==id)['arguments']['argv'][1]=={'$ref':'input.domain'}
 if len(sys.argv)>2 and sys.argv[2]:
  driver.execute_script("arguments[0].scrollIntoView({block:'center'})",node('fork'))
  driver.save_screenshot(sys.argv[2])
 # Removing a branch-to-join edge updates the wait list, rather than leaving a dead join.
 pick('dns')
 driver.find_element(By.CSS_SELECTOR,'[aria-label="断开 dns 到 join next"]').click()
 checked=check();assert next(n for n in checked['nodes'] if n['id']=='join')['from']==['http']
 layouts=[]
 for width in [1280,900]:
  driver.set_window_size(width,1500)
  wait.until(lambda d:d.execute_script('return window.innerWidth')==width)
  layout=driver.execute_script('''const canvas=document.querySelector('[data-testid="flow-canvas"]'),panel=document.querySelector('[data-testid="flow-node-panel"]');
  const c=canvas.getBoundingClientRect(),p=panel.getBoundingClientRect(),r=canvas.parentElement.getBoundingClientRect();
  return {width:innerWidth,canvas:c.height,panel:p.height,row:r.height,panel_top:p.top,canvas_bottom:c.bottom,overflow:getComputedStyle(panel).overflowY};''')
  assert layout['overflow']=='auto',layout
  if width>=1024: assert abs(layout['row']-layout['canvas'])<2,layout
  else: assert layout['panel_top']>=layout['canvas_bottom']-1 and layout['panel']<=540,layout
  layouts.append(layout)
 errors=[item for item in driver.get_log('browser') if item['level']=='SEVERE' or '[React Flow]' in item['message']]
 assert not errors,errors
 print(json.dumps({'dragged_fanout':2,'rendered_edges':6,'keyboard_delete':True,'sidebar_connect':True,'direct_bash':True,'domain_references':True,'join_sync':True,'unified_tool_entry':True,'canvas_layout':geometry,'responsive_layouts':layouts}))
except Exception:
 print('browser logs:',driver.get_log('browser'))
 print('rendered text:',driver.find_element(By.TAG_NAME,'body').text)
 raise
finally:
 driver.quit();server.shutdown()
`;
  const noProxy = [process.env.NO_PROXY, "localhost", "127.0.0.1"].filter(Boolean).join(",");
  const output = await new Promise((resolve) => {
    const child = spawn("python3", ["-c", python, html, process.env.SCOPEFORGE_FLOW_SCREENSHOT ?? ""], {
      env: { ...process.env, NO_PROXY: noProxy, no_proxy: noProxy },
    });
    let stdout = "",
      stderr = "";
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("error", (error) => resolve({ code: -1, stdout, stderr: String(error) }));
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
  assert.equal(output.code, 0, output.stdout + "\n" + output.stderr);
  console.log(output.stdout.trim());
});
