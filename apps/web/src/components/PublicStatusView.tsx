import type { CSSProperties } from 'react';

import { ImpactBadge, IncidentStatusBadge } from '@/components/IncidentBadges';
import { IncidentTimeline } from '@/components/IncidentTimeline';
import { OverallStatusBanner } from '@/components/OverallStatusBanner';
import { StatusBadge } from '@/components/StatusBadge';
import { UptimeBars } from '@/components/UptimeBars';
import type {
  PublicIncident,
  PublicService,
  PublicStatusPage,
} from '@/lib/api-types';
import { formatDateTime, incidentDuration, staleLabel } from '@/lib/format';
import styles from './PublicStatusView.module.css';

/**
 * Status sayfasının GÖRÜNÜMÜ — tek bir yerde (5.3'te ayrıldı).
 *
 * İki yerden kullanılıyor:
 *   1. `/status/[slug]` — ziyaretçinin gördüğü sayfa (sunucuda render ediliyor).
 *   2. Ayarlar ekranının CANLI ÖNİZLEMESİ (`/status-page`).
 *
 * Aynı bileşen olması şart: önizleme "buna benzer bir şey" değil, sayfanın
 * KENDİSİ olmalı. İkinci bir düzen yazsaydık, altı ay sonra önizlemede
 * göründüğü gibi yayımlanmayan bir sayfa kaçınılmazdı — ve o farkı ancak
 * yayımladıktan sonra fark ederdiniz.
 *
 * TEMA RENGİ TEK BİR DEĞİŞKENE BAĞLANIYOR (`--u-brand`), bileşenlere ham değer
 * olarak serpilmiyor. Kullanıldığı yerler tek bir CSS dosyasından okunabiliyor
 * ve rengi olmayan sayfada varsayılanı `var(--u-accent)`.
 *
 * KONTRAST: kullanıcının seçtiği renk HİÇBİR ZAMAN BİLGİ TAŞIMIYOR — durum her
 * yerde 0.2'nin sabit token'larıyla, ayrıca şekil ve metinle anlatılıyor. Renk
 * yalnızca kimlik (logo yeri, ince vurgu çizgisi, grup başlığı kenarı). Okunmaz
 * bir seçim sayfayı çirkinleştirebilir; yanlış ya da anlaşılmaz yapamaz.
 * Rengin ÜZERİNE yazılan tek metin (logo yoksa görünen baş harf) için ton
 * sunucudan geliyor (`themeTone`) ve burada mevcut metin token'ına çevriliyor.
 */

/** Tema değişkenlerini taşıyan stil nesnesi. */
type BrandStyle = CSSProperties & Record<'--u-brand' | '--u-brand-on', string>;

function brandStyle(page: PublicStatusPage): BrandStyle | undefined {
  if (!page.themeColor) return undefined;

  return {
    '--u-brand': page.themeColor,
    // Ham renk DEĞİL, token: sunucu hangi tonun okunur olduğunu söylüyor,
    // hangi değerin o ton olduğuna tasarım sistemi karar veriyor.
    '--u-brand-on':
      page.themeTone === 'dark' ? 'var(--u-text-on-accent)' : 'var(--u-text)',
  };
}

export function PublicStatusView({ page }: { page: PublicStatusPage }) {
  const active = page.incidents.filter(
    (incident) => incident.resolvedAt === null,
  );
  const past = page.incidents.filter(
    (incident) => incident.resolvedAt !== null,
  );

  return (
    <div className={styles.page} style={brandStyle(page)}>
      {/* Marka çizgisi: yalnızca dekorasyon, hiçbir bilgi taşımıyor. */}
      <div className={styles.brandBar} aria-hidden="true" />

      <header className={styles.header}>
        <PageMark page={page} />

        <h1 className={styles.title}>{page.title}</h1>

        {page.description ? (
          <p className={styles.description}>{page.description}</p>
        ) : (
          <p className={styles.subtitle}>Servis durumu</p>
        )}
      </header>

      <OverallStatusBanner overall={page.overall} />

      <StaleNotice count={page.staleServices} />

      <MaintenanceNotices page={page} />

      <section className={styles.section} aria-labelledby="servisler">
        <h2 className={styles.sectionTitle} id="servisler">
          Servisler
        </h2>

        {page.services.length === 0 ? (
          <p className={styles.empty}>
            Bu sayfada henüz yayımlanmış bir servis yok.
          </p>
        ) : (
          <ServiceGroups page={page} />
        )}
      </section>

      <section className={styles.section} aria-labelledby="aktif-kesintiler">
        <h2 className={styles.sectionTitle} id="aktif-kesintiler">
          Aktif kesintiler
        </h2>

        {active.length === 0 ? (
          <p className={styles.empty}>Devam eden bir kesinti yok.</p>
        ) : (
          <ul className={styles.incidents}>
            {active.map((incident) => (
              <IncidentCard key={incident.id} incident={incident} />
            ))}
          </ul>
        )}
      </section>

      <section className={styles.section} aria-labelledby="gecmis">
        <h2 className={styles.sectionTitle} id="gecmis">
          Son kesintiler
        </h2>

        {past.length === 0 ? (
          <p className={styles.empty}>
            Son 30 günde kapanmış bir kesinti kaydı yok.
          </p>
        ) : (
          <ul className={styles.incidents}>
            {past.map((incident) => (
              <IncidentCard key={incident.id} incident={incident} />
            ))}
          </ul>
        )}
      </section>

      <footer className={styles.footer}>
        <p>
          Bu sayfa <strong>Uptime Mini</strong> ile üretiliyor. Durumlar
          zamanlanmış kontrollere dayanıyor.
        </p>
      </footer>
    </div>
  );
}

/**
 * Sayfanın işareti: logo varsa görsel, yoksa marka renginde bir baş harf.
 *
 * `<img>` kullanılıyor (`next/image` değil): adres kullanıcıdan geliyor ve
 * `next/image` her alan adının yapılandırılmasını istiyor — status page'ini
 * kuran kişinin CDN'ini önceden bilemeyiz. `referrerPolicy` ile ziyaretçinin
 * hangi sayfadan geldiği o sunucuya söylenmiyor.
 */
function PageMark({ page }: { page: PublicStatusPage }) {
  if (page.logoUrl) {
    return (
      // eslint-disable-next-line @next/next/no-img-element -- kullanıcı adresi
      <img
        className={styles.logo}
        src={page.logoUrl}
        alt=""
        referrerPolicy="no-referrer"
      />
    );
  }

  return (
    <span className={styles.mark} aria-hidden="true">
      {page.title.trim().charAt(0).toLocaleUpperCase('tr-TR')}
    </span>
  );
}

/**
 * Servisler, gruplarıyla.
 *
 * Grupların SIRASI sunucudan (`page.groups`); gruplanmamış servisler en sonda
 * ve başlıksız. Servisi olmayan bir grup çizilmiyor — boş bir başlık ziyaretçi
 * için anlamsız (sorgu grupları elemiyor, karar burada: bkz.
 * ListPublicStatusPageGroups).
 */
function ServiceGroups({ page }: { page: PublicStatusPage }) {
  const grouped = page.groups
    .map((name) => ({
      name,
      services: page.services.filter((service) => service.group === name),
    }))
    .filter((group) => group.services.length > 0);

  const ungrouped = page.services.filter((service) => service.group === null);

  return (
    <>
      {grouped.map((group) => (
        <div key={group.name} className={styles.group}>
          <h3 className={styles.groupName}>{group.name}</h3>
          <ServiceList services={group.services} />
        </div>
      ))}

      {ungrouped.length > 0 && <ServiceList services={ungrouped} />}
    </>
  );
}

/**
 * SAYFA ÜSTÜ BAYATLIK NOTU (8.6, kullanıcı kararı).
 *
 * NEDEN BANDIN YANINA AYRI BİR NOT: karışık durumda (üç sağlam + bir bayat)
 * `Summarize` `operational` döndürüyor ve bant "Tüm sistemler çalışıyor"
 * diyor. Bayat servisin SATIRI dürüst — kendi rozetinde "ölçüm yok" yazıyor —
 * ama BAŞLIK değil. Bu not o boşluğu kapatıyor ve 5.1'in `overall`
 * sözleşmesine hiç dokunmuyor (enum'a yeni değer eklenmedi).
 *
 * Bilgi renkte değil METİNDE (0.2'nin şartı); kesikli kenarlık 8.2/8.5'in
 * uyarılarıyla aynı görsel dil.
 */
function StaleNotice({ count }: { count: number }) {
  if (count === 0) return null;

  return (
    <p className={styles.staleNotice} role="status">
      <strong>{count} servis şu anda ölçülemiyor.</strong> Bu servislerin durumu
      güncel olmayabilir; en son bilinen bilgiyi gösteriyoruz.
    </p>
  );
}

function ServiceList({ services }: { services: PublicService[] }) {
  return (
    <ul className={styles.services}>
      {services.map((service) => (
        <li key={service.id} className={styles.service}>
          <div className={styles.serviceHead}>
            <span className={styles.serviceName}>{service.name}</span>

            <span className={styles.serviceState}>
              {/* Bakım işareti METİN: renk tek sinyal değil (0.2). */}
              {service.inMaintenance && (
                <span className={styles.maintenanceTag}>Planlı bakım</span>
              )}
              {/* BAYAT SERVİS (8.6): son bilinen durum yerine "ölçüm yok"
                  rozeti. Ziyaretçiye "Çalışıyor" yazmak, tam olarak
                  kapatmaya çalıştığımız körlüğü sürdürmek olurdu. */}
              <StatusBadge
                status={service.status}
                staleLabel={
                  service.stale ? staleLabel(service.staleSince) : undefined
                }
              />
            </span>
          </div>

          {/* 90 günlük geçmiş (5.2): veri 3.4'ün günlük özetlerinden. */}
          <UptimeBars days={service.history} />
        </li>
      ))}
    </ul>
  );
}

/**
 * Bakım duyuruları.
 *
 * "Aktif mi yaklaşan mı" ayrımı İSTEMCİDE türetiliyor (4.3'ün kararı) — ama
 * servis satırındaki `inMaintenance` sunucudan geliyor; ikisi çelişmiyor çünkü
 * biri METNİ, diğeri GENEL ÖZETİ besliyor.
 */
function MaintenanceNotices({ page }: { page: PublicStatusPage }) {
  if (page.maintenance.length === 0) return null;

  return (
    <section className={styles.section} aria-labelledby="bakim">
      <h2 className={styles.sectionTitle} id="bakim">
        Planlı bakım
      </h2>

      <ul className={styles.maintenance}>
        {page.maintenance.map((window) => (
          <li key={window.id} className={styles.maintenanceItem}>
            <h3 className={styles.maintenanceTitle}>{window.title}</h3>

            <p className={`${styles.meta} tabularNumbers`}>
              {formatDateTime(window.startsAt)} →{' '}
              {formatDateTime(window.endsAt)}
            </p>

            {window.description && (
              <p className={styles.body}>{window.description}</p>
            )}

            <p className={styles.meta}>
              Etkilenen servisler:{' '}
              {window.services.map((service) => service.name).join(', ')}
            </p>
          </li>
        ))}
      </ul>
    </section>
  );
}

function IncidentCard({ incident }: { incident: PublicIncident }) {
  return (
    <li className={styles.incident}>
      <div className={styles.incidentHead}>
        <h3 className={styles.incidentTitle}>{incident.title}</h3>
        <IncidentStatusBadge status={incident.status} />
        <ImpactBadge impact={incident.impact} />
      </div>

      <p className={`${styles.meta} tabularNumbers`}>
        {formatDateTime(incident.startedAt)} · {incidentDuration(incident)}
      </p>

      {incident.services.length > 0 && (
        <p className={styles.meta}>
          Etkilenen servisler:{' '}
          {incident.services.map((service) => service.name).join(', ')}
        </p>
      )}

      <IncidentTimeline updates={incident.updates} />
    </li>
  );
}
