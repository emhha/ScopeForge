// Optional real-browser regression: make test-web-browser.
// Uses existing Selenium + Chromium/ChromeDriver, an isolated profile and a
// file:// fixture. It never starts an application server or authenticates.

import { build } from "vite";

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

test("browser renders original intent chain, on-line session labels, and node-scoped drawers", async (t) => {
  const result = await build({
    configFile: fileURLToPath(new URL("../../vite.config.mts", import.meta.url)),
    logLevel: "error",
    define: { "process.env.NODE_ENV": JSON.stringify("production") },
    build: {
      write: false,
      lib: {
        entry: fileURLToPath(new URL("./finding-lineage-browser.fixture.tsx", import.meta.url)),
        formats: ["iife"],
        name: "LineageRegression",
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
  assert.ok(js && css, "The browser fixture must contain both JS and actual graph styles");
  const dir = await mkdtemp(path.join(tmpdir(), "scopeforge-lineage-browser-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const htmlPath = path.join(dir, "fixture.html");
  await writeFile(
    htmlPath,
    `<!doctype html><html><head><meta charset="utf-8"><style>${css}</style></head><body><div id="root"></div><script>${js.replaceAll("</script", "<\\/script")}</script></body></html>`,
  );
  const python = String.raw`
import json, os, shutil, sys
from pathlib import Path
from selenium import webdriver
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.common.keys import Keys
from selenium.webdriver.support.ui import WebDriverWait

options = Options()
options.binary_location = shutil.which('chromium') or shutil.which('chromium-browser') or shutil.which('google-chrome')
assert options.binary_location, 'Chromium is required for this optional browser test'
for argument in ['--headless=new', '--no-sandbox', '--disable-dev-shm-usage', '--allow-file-access-from-files', '--window-size=2400,1000']:
    options.add_argument(argument)
options.set_capability('goog:loggingPrefs', {'browser': 'ALL'})
driver = webdriver.Chrome(service=Service(shutil.which('chromedriver')), options=options)
try:
    driver.execute_cdp_cmd('Network.enable', {})
    driver.execute_cdp_cmd('Network.setBlockedURLs', {'urls': ['http://*', 'https://*']})
    driver.get(Path(sys.argv[1]).as_uri())
    wait = WebDriverWait(driver, 20)
    wait.until(lambda d: len(d.find_elements(By.CSS_SELECTOR, '.react-flow__node')) == 12)
    wait.until(lambda d: len(d.find_elements(By.CSS_SELECTOR, '.react-flow__edge-annotated')) == 16)
    ids = {element.get_attribute('data-id') for element in driver.find_elements(By.CSS_SELECTOR, '.react-flow__node')}
    assert {'15', '124', '127', '161', '168', '126', '176'}.issubset(ids), ids
    assert '413' not in ids, 'Compression must not hide the original intents'
    labels = driver.find_elements(By.CSS_SELECTOR, '.react-flow__edgelabel-renderer > div')
    assert len(labels) == 16, len(labels)
    text = '\n'.join(element.text for element in labels)
    for intent in [124, 161, 168]:
        assert f'执行：意图会话 #{intent}' in text, text
        assert f'历史关联：意图会话 #{intent}' in text, text
    for element in labels:
        rect = element.rect
        assert rect['width'] > 0 and rect['height'] > 0, rect
        assert element.is_displayed(), 'Session label is present but invisible'
    def drawer(node, expected, excluded):
        driver.find_element(By.CSS_SELECTOR, f'.react-flow__node[data-id="{node}"]').click()
        dialog = wait.until(lambda d: d.find_element(By.CSS_SELECTOR, '[role="dialog"]'))
        wait.until(lambda d: f'intent:{expected}' in dialog.text)
        assert '此节点的来源与执行会话' in dialog.text, dialog.text
        for other in excluded:
            assert f'intent:{other}' not in dialog.text, dialog.text
        driver.find_element(By.TAG_NAME, 'body').send_keys(Keys.ESCAPE)
        wait.until(lambda d: not d.find_elements(By.CSS_SELECTOR, '[role="dialog"]'))
    drawer('126', 124, [15, 161, 168])
    drawer('161', 161, [124, 168])
    drawer('176', 168, [124, 161])
    screenshot = os.environ.get('LINEAGE_SCREENSHOT')
    if screenshot:
        driver.save_screenshot(screenshot)
    errors = [item for item in driver.get_log('browser') if item['level'] == 'SEVERE']
    assert not errors, errors
    print(json.dumps({'nodes': len(ids), 'annotated_edges': len(labels), 'node_scoped_drawers': [126, 161, 176], 'screenshot': screenshot}, ensure_ascii=False))
except Exception:
    print('browser logs:', json.dumps(driver.get_log('browser'), ensure_ascii=False))
    print('rendered text:', driver.find_element(By.TAG_NAME, 'body').text)
    if os.environ.get('LINEAGE_SCREENSHOT'):
        driver.save_screenshot(os.environ['LINEAGE_SCREENSHOT'])
    raise
finally:
    driver.quit()
`;
  const resultBrowser = spawnSync("python3", ["-c", python, htmlPath], { encoding: "utf8", timeout: 60000 });
  assert.equal(
    resultBrowser.status,
    0,
    `${resultBrowser.stdout}\n${resultBrowser.stderr}\n${resultBrowser.error ?? ""}`,
  );
  console.log(resultBrowser.stdout.trim());
});
