import { defineConfig, devices } from '@playwright/test';

import { API_URL, WEB_PORT, WEB_URL } from './support/stack';

/**
 * E2E yapılandırması (7.2).
 *
 * Yığının API ve WORKER tarafını `global-setup.ts` kaldırıyor (yerel süreçler
 * — gerekçesi support/stack.ts'te). WEB tarafı burada: Playwright'ın kendi
 * `webServer`i hazır olmayı bekleme ve kapatma işini zaten yapıyor.
 */
export default defineConfig({
  testDir: './tests',
  globalSetup: './global-setup.ts',

  /**
   * TEK WORKER, PARALELLİK YOK: testler tek bir veritabanını ve tek bir
   * worker sürecini paylaşıyor. Paralel koşan iki spec birbirinin
   * monitor'lerini kontrol ettirir ve "kaç kesinti var" sorusunu belirsiz
   * hâle getirirdi.
   */
  workers: 1,
  fullyParallel: false,

  /**
   * Kesinti akışı gerçek zaman bekliyor: monitor aralığının alt sınırı
   * 30 saniye (2.1) ve durum ancak bir sonraki turda değişiyor.
   */
  timeout: 120_000,
  expect: { timeout: 30_000 },

  // CI'da `test.only` kalmış bir dosya turu sessizce daraltmasın.
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,

  reporter: process.env.CI
    ? [['list'], ['html', { open: 'never' }]]
    : [['list'], ['html', { open: 'never' }]],

  use: {
    baseURL: WEB_URL,
    // Kırılan bir E2E'nin sebebi çoğu zaman ekranda: iz ve ekran görüntüsü
    // yalnızca BAŞARISIZ koşularda saklanıyor (7.3 bunları artifact olarak
    // yüklüyor).
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },

  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],

  webServer: {
    /**
     * ÜRETİM BUILD'İ, dev sunucusu değil: dev sunucusu rotaları ilk istekte
     * derliyor ve ilk gezinme 10-20 saniye sürebiliyor — E2E'nin gördüğü
     * gecikme ürünün değil derleyicinin olurdu.
     */
    command: `pnpm --filter web build && pnpm --filter web exec next start -p ${WEB_PORT}`,
    url: `${WEB_URL}/login`,
    timeout: 240_000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      // NEXT_PUBLIC_ öneki BUILD ZAMANINDA gömülüyor: burada verilmezse
      // tarayıcı 3400'deki GELİŞTİRME api'sine konuşurdu.
      NEXT_PUBLIC_API_URL: API_URL,
      // SSR yolunun api adresi (5.1) — public status sayfasını Next sunucusu
      // çekiyor, tarayıcı değil.
      API_INTERNAL_URL: API_URL,
    },
  },
});
