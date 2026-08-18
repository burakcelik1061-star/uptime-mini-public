import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  // Next 16'nın dev sunucusu her açılışta apps/web içine otomatik
  // markdown dosyaları üretiyor. Bu depo scaffold artığı tutmuyor (0.2'nin
  // kararı), bu yüzden üretim kapatıldı — kaldırılırsa dosyalar her
  // `next dev`de geri gelir.
  agentRules: false,
};

export default nextConfig;
