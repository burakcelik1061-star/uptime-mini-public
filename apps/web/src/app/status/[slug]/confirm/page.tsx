import Link from 'next/link';
import type { Metadata } from 'next';

import { confirmSubscription } from '@/lib/public-api';
import styles from '../../outcome.module.css';

/**
 * Abonelik onay sayfası (Faz 6.4) — çift onayın ikinci adımı.
 *
 * SUNUCU BİLEŞENİ: token adres satırından geliyor, api çağrısını Next sunucusu
 * yapıyor. Ziyaretçinin tarayıcısı api'ye yine hiç istek atmıyor (5.1'in
 * kararı) ve sayfa JavaScript olmadan çalışıyor.
 *
 * LİNKE TIKLAMAK ONAYLIYOR: çift onayın standart akışı bu. Token tek
 * kullanımlık olduğu için (sunucuda yakılıyor) ikinci tıklama "geçersiz veya
 * daha önce kullanılmış" mesajını alıyor — geçerli token uzayı hakkında bilgi
 * vermeyen tek bir cevap.
 */

export const metadata: Metadata = {
  title: 'Abonelik onayı',
  // Onay/iptal sayfaları arama sonuçlarında görünmemeli: içerikleri kişiye ve
  // token'a özgü.
  robots: { index: false, follow: false },
};

export default async function ConfirmSubscriptionPage({
  params,
  searchParams,
}: PageProps<'/status/[slug]/confirm'>) {
  const { slug } = await params;
  const query = await searchParams;

  const token = Array.isArray(query.token) ? query.token[0] : query.token;
  const result = token ? await confirmSubscription(token) : 'invalid';

  return (
    <main className={styles.page}>
      <h1 className={styles.title}>
        {result === 'ok' ? 'Aboneliğiniz onaylandı' : 'Bağlantı işlenemedi'}
      </h1>

      <p className={styles.body}>
        {result === 'ok' &&
          'Bundan sonra bir kesinti açıldığında ve kapandığında e-posta alacaksınız. Her bildirimde abonelikten çıkma bağlantısı bulunur.'}
        {result === 'invalid' &&
          'Bağlantı geçersiz veya daha önce kullanılmış. Status sayfasından yeniden abone olabilirsiniz.'}
        {result === 'error' &&
          'Şu anda işlenemedi. Biraz sonra bağlantıya yeniden tıklayın.'}
      </p>

      <Link className={styles.link} href={`/status/${slug}`}>
        Status sayfasına dön
      </Link>
    </main>
  );
}
