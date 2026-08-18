import Link from 'next/link';

import type { Incident } from '@/lib/api-types';
import { formatDateTime, incidentDuration } from '@/lib/format';
import { ImpactBadge, IncidentStatusBadge } from './IncidentBadges';
import styles from './IncidentTable.module.css';

/**
 * Kesinti listesi (4.4).
 *
 * Eylemler (güncelleme yazma, kapatma) BURADA DEĞİL, DETAY SAYFASINDA —
 * 2.4'ün monitor listesi için verdiği kararın aynısı: bir kaydın anlatısını
 * göstermeden yanına "kapat" düğmesi koymak listeyi kaza yüzeyine çevirir.
 *
 * "Kaynak" kolonu otomatik/manuel ayrımını gösteriyor: aynı listede iki farklı
 * köken var ve hangisinin nereden geldiği okunabilmeli (0.1 §5).
 */
export function IncidentTable({ incidents }: { incidents: Incident[] }) {
  if (incidents.length === 0) {
    return (
      <p className={styles.empty}>
        Bu süzgeçte kesinti kaydı yok. Otomatik incident&apos;lar bir monitor
        ardışık hata eşiğini geçtiğinde açılır.
      </p>
    );
  }

  return (
    <div className={styles.wrapper}>
      <table className={styles.table}>
        <caption className={styles.caption}>
          Organizasyonun kesinti kayıtları (yeniden eskiye)
        </caption>
        <thead>
          <tr>
            <th scope="col">Durum</th>
            <th scope="col">Başlık</th>
            <th scope="col">Etki</th>
            <th scope="col">Etkilenen servisler</th>
            <th scope="col">Başlangıç</th>
            <th scope="col">Süre</th>
            <th scope="col">Kaynak</th>
          </tr>
        </thead>
        <tbody>
          {incidents.map((incident) => (
            <tr key={incident.id}>
              <td>
                <IncidentStatusBadge status={incident.status} />
              </td>
              <td className={styles.title}>
                <Link href={`/incidents/${incident.id}`}>{incident.title}</Link>
              </td>
              <td>
                <ImpactBadge impact={incident.impact} />
              </td>
              <td className={styles.services}>
                {incident.monitors.length === 0 ? (
                  // Monitor silinmiş olabilir: incident kalıyor (0.1 §7).
                  <span className={styles.muted}>servis kaldırılmış</span>
                ) : (
                  incident.monitors.map((monitor) => monitor.name).join(', ')
                )}
              </td>
              <td className="tabularNumbers">
                {formatDateTime(incident.startedAt)}
              </td>
              <td className="tabularNumbers">{incidentDuration(incident)}</td>
              <td>{incident.isAutoCreated ? 'Otomatik' : 'Manuel'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
