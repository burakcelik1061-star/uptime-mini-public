import type { PublicIncidentUpdate } from '@/lib/api-types';
import { formatDateTime } from '@/lib/format';
import { IncidentStatusBadge } from './IncidentBadges';
import styles from './IncidentTimeline.module.css';

/**
 * Incident'ın anlatısı (4.2/4.4).
 *
 * KRONOLOJİK, eskiden yeniye: bir kesintinin hikayesi yukarıdan aşağıya
 * okunur — kontrol geçmişinin (2.3) tersine, orada "son ne oldu" sorulur.
 * Sunucu da bu sırada gönderiyor; istemci yeniden sıralamıyor.
 *
 * SİSTEM satırları ayrıca işaretli (4.1'in otomatik açma/kapaması): "bunu bir
 * insan mı yazdı" sorusunun cevabı görünür olmalı — ve yalnızca renkle değil,
 * METİNLE ("Sistem").
 *
 * Prop tipi 5.1'de DARALTILDI (`PublicIncidentUpdate`): bileşen yazarın
 * kimliğini zaten kullanmıyordu ve public sözleşme `authorId` taşımıyor.
 * Böylece aynı timeline hem admin ekranında hem public status sayfasında
 * çalışıyor — ikinci bir kopya, altı ay sonra ayrışan iki anlatı demekti.
 */
export function IncidentTimeline({
  updates,
}: {
  updates: PublicIncidentUpdate[];
}) {
  if (updates.length === 0) {
    return (
      <p className={styles.empty}>
        Bu incident&apos;ın timeline&apos;ı boş. (4.2&apos;den önce açılmış
        kayıtlarda sistem satırı bulunmuyor.)
      </p>
    );
  }

  return (
    <ol className={styles.timeline}>
      {updates.map((update) => (
        <li key={update.id} className={styles.item}>
          <div className={styles.head}>
            <IncidentStatusBadge status={update.status} />
            <time
              className={`${styles.time} tabularNumbers`}
              dateTime={update.createdAt}
            >
              {formatDateTime(update.createdAt)}
            </time>
            <span className={styles.author}>
              {update.isSystem ? 'Sistem' : 'Ekip'}
            </span>
          </div>
          <p className={styles.body}>{update.body}</p>
        </li>
      ))}
    </ol>
  );
}
