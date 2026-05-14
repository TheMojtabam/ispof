// Capture screenshots from the live Ispof panel backed by a real ispof
// binary serving real data from /tmp/ispof-test-env.
//
// usage: node screenshot.js [base_url] [output_dir]
const { chromium } = require('/home/claude/.npm-global/lib/node_modules/playwright');
const path = require('path');

const baseUrl  = process.argv[2] || 'http://127.0.0.1:12095';
const outDir   = process.argv[3] || '/home/claude/release/ispof-v0.1.0/docs/screenshots';

(async () => {
  const browser = await chromium.launch({
    executablePath: '/opt/pw-browsers/chromium-1194/chrome-linux/chrome',
    args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
  });
  const page = await ctx.newPage();

  page.on('pageerror', (e) => console.error('[pageerror]', e.message));
  page.on('console', (m) => {
    if (m.type() === 'error') console.error('[console.error]', m.text());
  });

  const shoot = async (name, action) => {
    if (action) await action(page);
    // wait for live updates to settle
    await page.waitForTimeout(800);
    const file = path.join(outDir, `${name}.png`);
    await page.screenshot({ path: file, fullPage: false });
    console.log(`  ✓ ${name}.png`);
  };

  console.log(`Loading ${baseUrl}/...`);
  await page.goto(baseUrl, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.brand', { timeout: 10000 });
  // Wait for several SSE ticks so the rate cache has prev values and the
  // history buffer has enough points to draw a meaningful chart.
  await page.waitForTimeout(8000);

  // 1. Dashboard
  await shoot('01-dashboard');

  // 2. Tunnels list
  await shoot('02-tunnels', async (p) => {
    await p.click('.nav-item[data-view="tunnels"]');
    await p.waitForTimeout(600);
  });

  // 3. Tunnel detail — overview
  await shoot('03-tunnel-detail-overview', async (p) => {
    // Click the first tunnel row
    await p.click('#tunnels-table tbody tr:first-child');
    await p.waitForTimeout(600);
  });

  // 4. Tunnel detail — config tab
  await shoot('04-tunnel-detail-config', async (p) => {
    await p.click('.subtab[data-sub="config"]');
    await p.waitForTimeout(600);
  });

  // 5. Tunnel detail — logs (real journalctl call — empty since no daemon)
  await shoot('05-tunnel-detail-logs', async (p) => {
    await p.click('.subtab[data-sub="logs"]');
    await p.waitForTimeout(900);
  });

  // 6. Tools — keygen
  await shoot('06-tools-keygen', async (p) => {
    await p.click('.nav-item[data-view="tools"]');
    await p.waitForTimeout(400);
    await p.click('button.btn.btn-primary:has-text("Generate key pair")').catch(() => {});
    await p.waitForTimeout(800);
  });

  // 7. New tunnel wizard (step 1)
  await shoot('07-new-tunnel-wizard', async (p) => {
    await p.click('.nav-item[data-view="tunnels"]');
    await p.waitForTimeout(400);
    // Find a "+ New Tunnel" button
    const newBtn = await p.$('button:has-text("New Tunnel")');
    if (newBtn) await newBtn.click();
    await p.waitForTimeout(700);
  });

  // 8. Settings
  await shoot('08-settings', async (p) => {
    // close any open modal first via JS (Escape doesn't have a handler)
    await p.evaluate(() => {
      const m = document.getElementById('new-tunnel-modal');
      if (m) m.classList.remove('open');
    });
    await p.waitForTimeout(200);
    await p.click('.nav-item[data-view="settings"]');
    await p.waitForTimeout(900);
  });

  await browser.close();
  console.log(`\n→ saved to ${outDir}/`);
})();
