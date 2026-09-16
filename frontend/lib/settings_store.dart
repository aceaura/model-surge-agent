/// 服务地址与管理密钥的本机持久化。
library;

import 'package:shared_preferences/shared_preferences.dart';

class Settings {
  const Settings({required this.baseUrl, required this.adminKey});

  final String baseUrl;
  final String adminKey;

  bool get complete => baseUrl.isNotEmpty && adminKey.isNotEmpty;

  static const empty = Settings(baseUrl: '', adminKey: '');
}

class SettingsStore {
  static const _baseUrlKey = 'msa.base_url';
  static const _adminKeyKey = 'msa.admin_key';

  Future<Settings> load() async {
    final prefs = await SharedPreferences.getInstance();
    return Settings(
      baseUrl: prefs.getString(_baseUrlKey) ?? '',
      adminKey: prefs.getString(_adminKeyKey) ?? '',
    );
  }

  Future<void> save(Settings settings) async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setString(_baseUrlKey, settings.baseUrl);
    await prefs.setString(_adminKeyKey, settings.adminKey);
  }
}

/// mask 用于界面显示密钥：短值全掩，长值保留前后各 4 位。
/// 与服务端脱敏规则一致，避免同一密钥在两处显示不同。
String mask(String value) {
  if (value.isEmpty) return '';
  if (value.length <= 8) return '***';
  return '${value.substring(0, 4)}***${value.substring(value.length - 4)}';
}
