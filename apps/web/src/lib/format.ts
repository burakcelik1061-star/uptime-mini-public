/**
 * Görüntüleme yardımcıları (Faz 4.4).
 *
 * Bu dosya BİÇİMLENDİRME yapıyor, HESAP yapmıyor: kesinti süresi sunucudan
 * saniye olarak geliyor (`durationSeconds`), uptime yüzdesi sunucuda
 * hesaplanıyor (2.4). Buradaki tek istisna DEVAM EDEN kesintinin süresi —
 * onun bitişi henüz yok, "şu an" istemcinin saati.
 */

/** Zaman damgasını kullanıcının yerelinde okunur hâle getirir. */
export function formatDateTime(iso: string): string {
  return new Date(iso).toLocaleString('tr-TR', {
    dateStyle: 'medium',
    timeStyle: 'short',
  });
}

/**
 * Süreyi kabaca yazar: "2 gün 3 sa", "1 sa 9 dk", "45 sn".
 *
 * Saniyeler yalnızca bir dakikanın altındayken gösteriliyor: "1 sa 9 dk 12 sn"
 * bir kesinti raporunda taşıdığından fazla hassasiyet iddia ederdi (kontrol
 * aralığı zaten 30 saniyenin altına inemiyor).
 */
export function formatDuration(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds));

  if (seconds < 60) return `${seconds} sn`;

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} dk`;

  const hours = Math.floor(minutes / 60);
  const restMinutes = minutes % 60;
  if (hours < 24) {
    return restMinutes === 0 ? `${hours} sa` : `${hours} sa ${restMinutes} dk`;
  }

  const days = Math.floor(hours / 24);
  const restHours = hours % 24;

  return restHours === 0 ? `${days} gün` : `${days} gün ${restHours} sa`;
}

/**
 * Kesintinin süresi.
 *
 * Kapanmışsa SUNUCUNUN hesabı (`durationSeconds`) kullanılıyor — iki zaman
 * damgasını istemcide çıkarmak, her istemcinin aynı kararı yeniden vermesi
 * demekti. Sürüyorsa "şu ana kadar" hesabı burada, çünkü sunucunun bir bitişi
 * yok ve onun "şu an"ı istemcininkiyle çakışmayabilir.
 */
export function incidentDuration(incident: {
  startedAt: string;
  durationSeconds: number | null;
}): string {
  if (incident.durationSeconds !== null) {
    return formatDuration(incident.durationSeconds);
  }

  const elapsed = (Date.now() - new Date(incident.startedAt).getTime()) / 1000;

  return `${formatDuration(elapsed)} (sürüyor)`;
}

/**
 * Bakım penceresinin şimdiye göre konumu.
 *
 * SUNUCUDAN GELMİYOR (4.3'ün kararı): üçü de `startsAt`/`endsAt` ile
 * türetilebiliyor ve sunucunun "şu an"ı ile istemcininki arasındaki fark, aynı
 * pencerenin bir yerde "yaklaşan" başka bir yerde "aktif" görünmesi demekti.
 */
export type MaintenancePhase = 'upcoming' | 'active' | 'past';

export function maintenancePhase(window: {
  startsAt: string;
  endsAt: string;
}): MaintenancePhase {
  const now = Date.now();
  const start = new Date(window.startsAt).getTime();
  const end = new Date(window.endsAt).getTime();

  if (now < start) return 'upcoming';
  // Yarı açık aralık [starts, ends): bitiş anında pencere artık aktif değil.
  if (now < end) return 'active';

  return 'past';
}

/**
 * "N dakikadır ölçüm yok" etiketi (8.6).
 *
 * KARAR SUNUCUNUN, METİN İSTEMCİNİN: bir monitor'ün bayat OLUP OLMADIĞINA
 * sunucu karar veriyor (`stale`) — kural tek yerde kalsın ve tarayıcının
 * saati yanlışsa sonuç değişmesin. Buradaki iş yalnızca geçen süreyi
 * okunabilir hâle getirmek (4.4'ün "devam eden kesintinin 'şu ana kadar'ı
 * istemcide" bölüşümünün aynısı).
 *
 * `formatDuration` KULLANILMIYOR: o kısaltma üretiyor ("14 dk") ve sonuna
 * ek geldiğinde bozuk bir kelime çıkıyor ("14 dkdır"). Bu etiket bir cümlenin
 * parçası, tablo hücresi değil — birim AÇIK yazılıyor ve Türkçe ek birime
 * göre değişiyor.
 *
 * `since` yoksa süre yazılmıyor: alan bilinmiyorken "0 dakikadır" demek,
 * olmayan bir kesinliği ima etmek olurdu.
 */
export function staleLabel(since: string | null | undefined): string {
  if (!since) return 'Ölçüm yok';

  const seconds = Math.max(0, (Date.now() - new Date(since).getTime()) / 1000);

  const [amount, unit] =
    seconds < 60
      ? [Math.floor(seconds), 'saniyedir']
      : seconds < 3600
        ? [Math.floor(seconds / 60), 'dakikadır']
        : seconds < 86400
          ? [Math.floor(seconds / 3600), 'saattir']
          : [Math.floor(seconds / 86400), 'gündür'];

  return `${amount} ${unit} ölçüm yok`;
}
