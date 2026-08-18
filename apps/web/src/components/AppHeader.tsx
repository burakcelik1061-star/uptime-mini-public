'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useEffect, useState } from 'react';

import type { Organization } from '@/lib/api-types';
import { useRealtime, type RealtimeStatus } from '@/lib/realtime';
import { useSession } from '@/lib/session';
import styles from './AppHeader.module.css';

/**
 * Panonun üst çubuğu ve bölüm gezinmesi (4.4).
 *
 * 4.4'e kadar üst çubuk yalnızca panoda vardı; incident ve bakım ekranları
 * gelince ORTAKLAŞTIRILDI — üç sayfada üç kopya, altı ay sonra ayrışan üç
 * navigasyon demekti (2.4'ün "düzenleme için ikinci bir form açma" kararıyla
 * aynı gerekçe).
 *
 * Aktif bölüm `aria-current` ile de işaretleniyor: seçili sekme yalnızca
 * renkle anlatılmıyor (0.2'nin şartı).
 */

const SECTIONS = [
  { href: '/', label: 'Monitörler' },
  { href: '/incidents', label: 'Kesintiler' },
  { href: '/maintenance', label: 'Bakım' },
  { href: '/status-page', label: 'Status page' },
  { href: '/notifications', label: 'Bildirimler' },
  { href: '/team', label: 'Ekip' },
  { href: '/plan', label: 'Plan' },
] as const;

/**
 * Canlı bağlantının üç hâli. DURUM RENKLE ANLATILMIYOR (0.2'nin şartı):
 * her hâlin ayrı ŞEKLİ (dolu/halka/çizgi) ve ayrı METNİ var.
 */
const CONNECTION_LABELS: Record<
  RealtimeStatus,
  { shape: string; label: string }
> = {
  idle: { shape: '–', label: 'Canlı yayın kapalı' },
  connecting: { shape: '◌', label: 'Bağlanıyor' },
  live: { shape: '●', label: 'Canlı' },
  offline: { shape: '○', label: 'Bağlantı koptu, yeniden deneniyor' },
};

function ConnectionBadge({ status }: { status: RealtimeStatus }) {
  const { shape, label } = CONNECTION_LABELS[status];

  return (
    <span
      className={`${styles.connection} ${styles[status]}`}
      // Durum sessizce değişiyor; ekran okuyucu bunu kesintiye uğramadan
      // duyursun diye kibar (polite) bir canlı bölge.
      role="status"
      aria-live="polite"
    >
      <span aria-hidden="true">{shape}</span>
      {label}
    </span>
  );
}

/**
 * useDevMailbox, geliştirici posta kutusunun (8.2) açık olup olmadığını
 * SUNUCUYA sorar.
 *
 * NEDEN BİR ORTAM DEĞİŞKENİ DEĞİL: kapı api'nin `APP_ENV`inde
 * (config.devToolsEnabled), arayüzün değil. `NEXT_PUBLIC_*` bir bayrak
 * eklemek, aynı kararın ikinci bir kopyası olurdu ve ikisi ayrıştığı gün
 * arayüz olmayan bir ekrana link verirdi (5.1'in "kuralın ikinci kopyası
 * olmasın" disiplini).
 *
 * Cevabı ne olursa olsun ERİŞİM sunucuda kapalı: bu yalnızca ölü bir
 * navigasyon linki göstermemek için. Sorgu oturum başına bir kez koşuyor
 * (üst çubuk düzenin içinde, sayfa geçişlerinde yeniden bağlanmıyor);
 * kapı kapalıyken cevap zaten küçük bir 404.
 */
function useDevMailbox(enabled: boolean): boolean {
  const { authFetch } = useSession();
  const [available, setAvailable] = useState(false);

  useEffect(() => {
    if (!enabled) return;

    let cancelled = false;

    authFetch('/v1/dev/outbox')
      .then(() => {
        if (!cancelled) setAvailable(true);
      })
      .catch(() => {
        // 404 (kapı kapalı) ve diğer her hata aynı sonuç: link gösterme.
        if (!cancelled) setAvailable(false);
      });

    return () => {
      cancelled = true;
    };
  }, [enabled, authFetch]);

  return available;
}

export function AppHeader({ org }: { org: Organization | null }) {
  const pathname = usePathname();
  const { status, user, logout } = useSession();
  const mailboxAvailable = useDevMailbox(status === 'authenticated');
  // Canlı yayın (6.1): pano açıkken org kanalına bağlı kalıyoruz. Olayları
  // tüketen ekranlar 6.2'de geliyor; buradaki gösterge bağlantının kendisini
  // görünür kılıyor.
  const connection = useRealtime(org?.id ?? null);

  return (
    <header className={styles.topbar}>
      <div className={styles.brand}>
        <span className={styles.logo}>Uptime Mini</span>
        {org && <span className={styles.org}>{org.name}</span>}
        <ConnectionBadge status={connection} />
      </div>

      <nav className={styles.nav} aria-label="Bölümler">
        {[
          ...SECTIONS,
          // POSTA KUTUSU YALNIZCA SUNUCU AÇIKSA LİSTEDE (8.2): ürünün bir
          // bölümü değil, geliştirme aracı — üretimde uç hiç kayıtlı olmadığı
          // için burada da görünmüyor.
          ...(mailboxAvailable
            ? [{ href: '/mailbox', label: 'Posta kutusu' } as const]
            : []),
        ].map((section) => {
          const active =
            section.href === '/'
              ? pathname === '/'
              : pathname.startsWith(section.href);

          return (
            <Link
              key={section.href}
              href={section.href}
              className={
                active ? `${styles.link} ${styles.active}` : styles.link
              }
              aria-current={active ? 'page' : undefined}
            >
              {section.label}
            </Link>
          );
        })}
      </nav>

      <div className={styles.account}>
        <span className={styles.email}>{user?.email}</span>
        <button
          className={styles.logout}
          type="button"
          onClick={() => void logout()}
        >
          Çıkış
        </button>
      </div>
    </header>
  );
}
