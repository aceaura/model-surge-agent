import 'package:flutter/material.dart';
import 'package:window_manager/window_manager.dart';

import 'api_client.dart';
import 'pages/dashboard_page.dart';
import 'pages/live_page.dart';
import 'pages/outbox_page.dart';
import 'pages/requests_page.dart';
import 'pages/settings_page.dart';
import 'settings_store.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await windowManager.ensureInitialized();
  await windowManager.waitUntilReadyToShow(
    const WindowOptions(
      size: Size(1360, 860),
      minimumSize: Size(1080, 700),
      title: 'ModelSurge Agent 数据面观测台',
      titleBarStyle: TitleBarStyle.normal,
    ),
    () async {
      await windowManager.show();
      await windowManager.focus();
    },
  );
  runApp(const AdminApp());
}

class AdminApp extends StatelessWidget {
  const AdminApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'ModelSurge Agent 数据面观测台',
      theme: ThemeData(colorSchemeSeed: Colors.indigo, useMaterial3: true),
      home: const AdminShell(),
    );
  }
}

/// AdminShell 只做三件事：读本机配置、按配置构造客户端、把客户端注入各页。
/// 配置不完整时渲染不可跳过的设置页，因此各页可以假定客户端已配置完整。
class AdminShell extends StatefulWidget {
  const AdminShell({super.key, this.store});

  final SettingsStore? store;

  @override
  State<AdminShell> createState() => _AdminShellState();
}

class _AdminShellState extends State<AdminShell> {
  late final SettingsStore _store = widget.store ?? SettingsStore();

  Settings? _settings;
  ApiClient? _client;
  int _tab = 0;

  @override
  void initState() {
    super.initState();
    _restore();
  }

  Future<void> _restore() async {
    final settings = await _store.load();
    if (!mounted) return;
    setState(() {
      _settings = settings;
      _client = settings.complete ? _clientFor(settings) : null;
    });
  }

  ApiClient _clientFor(Settings s) =>
      ApiClient(baseUrl: s.baseUrl, adminKey: s.adminKey);

  Future<void> _apply(Settings s) async {
    await _store.save(s);
    if (!mounted) return;
    setState(() {
      _client?.close();
      _settings = s;
      _client = _clientFor(s);
    });
  }

  void _openSettings() {
    final current = _settings ?? Settings.empty;
    Navigator.of(context).push(MaterialPageRoute(
      builder: (_) => SettingsPage(initial: current, onSaved: _apply),
    ));
  }

  @override
  void dispose() {
    _client?.close();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final settings = _settings;
    if (settings == null) {
      return const Scaffold(body: Center(child: CircularProgressIndicator()));
    }
    if (!settings.complete || _client == null) {
      return SettingsPage(
        initial: settings,
        onSaved: _apply,
        dismissible: false,
      );
    }

    final client = _client!;
    return Scaffold(
      appBar: AppBar(
        title: const Text('ModelSurge Agent 数据面观测台'),
        actions: [
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 8),
            child: Center(
              child: Text('${settings.baseUrl}  key ${mask(settings.adminKey)}'),
            ),
          ),
          IconButton(
            tooltip: '连接设置',
            icon: const Icon(Icons.settings),
            onPressed: _openSettings,
          ),
        ],
      ),
      body: Row(
        children: [
          NavigationRail(
            selectedIndex: _tab,
            labelType: NavigationRailLabelType.all,
            onDestinationSelected: (i) => setState(() => _tab = i),
            destinations: const [
              NavigationRailDestination(
                icon: Icon(Icons.speed_outlined),
                selectedIcon: Icon(Icons.speed),
                label: Text('总览'),
              ),
              NavigationRailDestination(
                icon: Icon(Icons.stream_outlined),
                selectedIcon: Icon(Icons.stream),
                label: Text('实时'),
              ),
              NavigationRailDestination(
                icon: Icon(Icons.receipt_long_outlined),
                selectedIcon: Icon(Icons.receipt_long),
                label: Text('流水'),
              ),
              NavigationRailDestination(
                icon: Icon(Icons.outbox_outlined),
                selectedIcon: Icon(Icons.outbox),
                label: Text('上报'),
              ),
            ],
          ),
          const VerticalDivider(width: 1),
          Expanded(
            // 各页自己轮询，切页即销毁定时器；不做全局轮询以免离开页面还在拉。
            child: switch (_tab) {
              1 => LiveFeedPage(client: client, onOpenSettings: _openSettings),
              2 => RequestsPage(client: client, onOpenSettings: _openSettings),
              3 => OutboxPage(client: client, onOpenSettings: _openSettings),
              _ => DashboardPage(client: client, onOpenSettings: _openSettings),
            },
          ),
        ],
      ),
    );
  }
}
