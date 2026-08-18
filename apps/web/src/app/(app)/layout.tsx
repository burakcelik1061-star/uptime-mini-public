import { SessionProvider } from '@/lib/session';

/**
 * Panonun (oturum gerektiren ekranların) yerleşimi (5.1'de ayrıldı).
 *
 * `SessionProvider` burada, kökte DEĞİL: sağlayıcı açılışta refresh
 * cookie'siyle oturum kurtarmayı deniyor ve bu, public status sayfasının
 * yapmaması gereken tek şey (bkz. app/layout.tsx). Route group parantezli
 * olduğu için ADRESLER DEĞİŞMİYOR: `/`, `/login`, `/monitors/[id]` aynı kaldı.
 *
 * Sağlayıcı ayrıca burada kalmak zorunda: sayfa geçişlerinde yeniden
 * kurulmuyor, yani oturum kurtarma her gezinmede tekrar koşmuyor (2.1).
 */
export default function AppLayout({ children }: LayoutProps<'/'>) {
  return <SessionProvider>{children}</SessionProvider>;
}
