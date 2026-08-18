/**
 * E2E turunun kurulumu: temiz veritabanı → şema → api → worker.
 *
 * Playwright globalSetup'tan dönen fonksiyonu TEARDOWN olarak çağırıyor;
 * süreçlerin listesi böylece aynı modül kapsamında kalıyor (ayrı bir
 * globalTeardown dosyası, listeyi diske yazıp geri okumayı gerektirirdi).
 */
import {
  buildBinaries,
  migrate,
  resetDatabase,
  startApi,
  startWorker,
  stopAll,
} from './support/stack';

export default async function globalSetup(): Promise<() => Promise<void>> {
  console.log('[e2e] veritabanı sıfırlanıyor (uptime_mini_e2e)…');
  await resetDatabase();

  console.log("[e2e] binary'ler derleniyor…");
  await buildBinaries();

  console.log("[e2e] migration'lar uygulanıyor…");
  await migrate();

  console.log('[e2e] api başlatılıyor…');
  await startApi();

  console.log('[e2e] worker başlatılıyor…');
  await startWorker();

  console.log('[e2e] yığın hazır.');

  return async () => {
    console.log('[e2e] api ve worker durduruluyor…');
    stopAll();
  };
}
