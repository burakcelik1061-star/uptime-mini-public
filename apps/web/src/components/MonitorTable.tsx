import Link from 'next/link';

import type { Monitor } from '@/lib/api-types';
import { staleLabel } from '@/lib/format';
import { StatusBadge } from './StatusBadge';
import styles from './MonitorTable.module.css';

/**
 * Monitor listesi.
 *
 * Düzenleme/silme/kontrol İŞLEMLERİ BURADA DEĞİL, DETAY SAYFASINDA (2.4):
 * bir satırın ne zaman ne yaptığını göstermeden yanına "sil" düğmesi koymak
 * listeyi kaza yüzeyine çevirirdi. Ad, detay sayfasına bağlantı.
 */
export function MonitorTable({ monitors }: { monitors: Monitor[] }) {
  if (monitors.length === 0) {
    return (
      <p className={styles.empty}>
        Henüz monitor yok. Yukarıdaki formla ilk kontrolünüzü ekleyin.
      </p>
    );
  }

  return (
    <div className={styles.wrapper}>
      <table className={styles.table}>
        <caption className={styles.caption}>
          Organizasyonun izlenen uçları
        </caption>
        <thead>
          <tr>
            <th scope="col">Durum</th>
            <th scope="col">Ad</th>
            <th scope="col">URL</th>
            <th scope="col">Metot</th>
            <th scope="col">Aralık</th>
            <th scope="col">Zaman aşımı</th>
            <th scope="col">Beklenen status</th>
          </tr>
        </thead>
        <tbody>
          {monitors.map((monitor) => (
            <tr key={monitor.id}>
              <td>
                {/* BAYAT MONITOR PANODA AÇIKÇA GÖRÜNÜYOR (8.6): worker öldüğünde
                    liste son bilinen durumu yeşil göstermeye devam ederdi. */}
                <StatusBadge
                  status={monitor.status}
                  staleLabel={
                    monitor.stale
                      ? staleLabel(monitor.lastCheckedAt)
                      : undefined
                  }
                />
              </td>
              <td className={styles.name}>
                <Link href={`/monitors/${monitor.id}`}>{monitor.name}</Link>
              </td>
              <td className={styles.url}>
                {/* Kullanıcının girdiği adres YENİ SEKMEDE ve
                    rel="noreferrer" ile açılıyor: hedef site referrer'dan
                    panelin adresini öğrenmesin. */}
                <a href={monitor.url} target="_blank" rel="noreferrer noopener">
                  {monitor.url}
                </a>
              </td>
              <td>{monitor.method}</td>
              <td className="tabularNumbers">
                {monitor.checkIntervalSeconds} sn
              </td>
              <td className="tabularNumbers">{monitor.timeoutMs} ms</td>
              <td className="tabularNumbers">
                {monitor.expectedStatusMin}–{monitor.expectedStatusMax}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
