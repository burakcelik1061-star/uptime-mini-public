import Link from 'next/link';
import type { Metadata } from 'next';

import { unsubscribeFromStatusPage } from '@/lib/public-api';
import styles from '../../outcome.module.css';

/**
 * Abonelikten çıkma sayfası (Faz 6.4).
 *
 * TEK TIKLAMAYLA BİTİYOR, onay ekranı yok: ayrılmak isteyen birine ikinci bir
 * engel koymak, aboneliği sonlandırmayı zorlaştırmak olur. Sayfa yine sunucu
 * bileşeni — istek Next sunucusundan api'ye gidiyor.
 *
 * İkinci tıklama da aynı cevabı alıyor: abone satırı SİLİNDİĞİ için token
 * artık eşleşmiyor ve "geçersiz veya zaten iptal edilmiş" mesajı dönüyor.
 */

export const metadata: Metadata = {
  title: 'Abonelikten çıkma',
  robots: { index: false, follow: false },
};

export default async function UnsubscribePage({
  params,
  searchParams,
}: PageProps<'/status/[slug]/unsubscribe'>) {
  const { slug } = await params;
  const query = await searchParams;

  const token = Array.isArray(query.token) ? query.token[0] : query.token;
  const result = token ? await unsubscribeFromStatusPage(token) : 'invalid';

  return (
    <main className={styles.page}>
      <h1 className={styles.title}>
        {result === 'ok' ? 'Aboneliğiniz sonlandırıldı' : 'Bağlantı işlenemedi'}
      </h1>

      <p className={styles.body}>
        {result === 'ok' &&
          'Bu adrese artık kesinti bildirimi gönderilmeyecek. Kaydınız tamamen silindi.'}
        {result === 'invalid' &&
          'Bağlantı geçersiz veya abonelik zaten iptal edilmiş.'}
        {result === 'error' &&
          'Şu anda işlenemedi. Biraz sonra bağlantıya yeniden tıklayın.'}
      </p>

      <Link className={styles.link} href={`/status/${slug}`}>
        Status sayfasına dön
      </Link>
    </main>
  );
}
