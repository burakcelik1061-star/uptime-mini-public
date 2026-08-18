import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Uptime Mini',
  description: 'Uptime monitoring ve public status page',
};

export default function RootLayout({ children }: LayoutProps<'/'>) {
  // Tek tema (koyu) bilinçli bir karar — bkz. docs/0.2-design-system.md §2.
  //
  // OTURUM SAĞLAYICISI ARTIK BURADA DEĞİL (5.1): uygulama iki ROUTE GROUP'a
  // ayrıldı.
  //
  //   (app)/  — pano, giriş, monitor/incident/bakım ekranları. Oturumu
  //             `SessionProvider` taşıyor: açılışta refresh cookie'siyle
  //             oturum kurtarma denemesi yapıyor (2.1).
  //   status/ — PUBLIC status sayfası. Oturumsuz ve SUNUCUDA render ediliyor.
  //
  // Ayrım şart, çünkü provider en dışta kalsaydı ziyaretçinin tarayıcısı her
  // public sayfa açılışında `POST /v1/auth/refresh` çağırırdı: oturumsuz bir
  // sayfadan yapılan, kimlik bilgisi taşıyan bir çapraz-origin istek — yani
  // "public sayfa CORS yoluna hiç girmiyor" kararının tam tersi.
  return (
    <html lang="tr">
      <body>{children}</body>
    </html>
  );
}
