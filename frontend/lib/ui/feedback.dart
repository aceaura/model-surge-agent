/// 错误展示与进行中状态的共用件。三类失败在此统一成可操作提示。
library;

import 'package:flutter/material.dart';

import '../api_client.dart';

/// describeError 把异常转成面向运维者的文案。
String describeError(Object error) => switch (error) {
      UnreachableException e => '${e.message}\n请确认服务已启动、地址可达。',
      UnauthorizedException _ => '管理密钥无效，请到设置页更新。',
      ApiErrorException e => e.message,
      _ => '$error',
    };

/// needsSettings 判断该错误是否应引导运维者去设置页。
bool needsSettings(Object error) =>
    error is UnauthorizedException || error is UnreachableException;

class ErrorPanel extends StatelessWidget {
  const ErrorPanel({
    super.key,
    required this.error,
    this.onRetry,
    this.onOpenSettings,
  });

  final Object error;
  final VoidCallback? onRetry;
  final VoidCallback? onOpenSettings;

  @override
  Widget build(BuildContext context) {
    return Center(
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 520),
        child: Card(
          child: Padding(
            padding: const EdgeInsets.all(24),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Row(
                  children: [
                    Icon(Icons.error_outline,
                        color: Theme.of(context).colorScheme.error),
                    const SizedBox(width: 8),
                    const Text('请求失败'),
                  ],
                ),
                const SizedBox(height: 12),
                SelectableText(describeError(error)),
                const SizedBox(height: 16),
                Row(
                  children: [
                    if (onRetry != null)
                      FilledButton(onPressed: onRetry, child: const Text('重试')),
                    if (onRetry != null && onOpenSettings != null)
                      const SizedBox(width: 8),
                    if (onOpenSettings != null && needsSettings(error))
                      OutlinedButton(
                        onPressed: onOpenSettings,
                        child: const Text('打开设置'),
                      ),
                  ],
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

/// showError 用于提交类操作的失败提示（列表已有内容，不该整页替换）。
void showError(BuildContext context, Object error) {
  ScaffoldMessenger.of(context)
      .showSnackBar(SnackBar(content: Text(describeError(error))));
}

void showInfo(BuildContext context, String message) {
  ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(message)));
}

/// BusyButton 在请求进行中禁用自身并显示进度，避免重复提交。
class BusyButton extends StatelessWidget {
  const BusyButton({
    super.key,
    required this.busy,
    required this.onPressed,
    required this.child,
  });

  final bool busy;
  final VoidCallback? onPressed;
  final Widget child;

  @override
  Widget build(BuildContext context) {
    return FilledButton(
      onPressed: busy ? null : onPressed,
      child: busy
          ? const SizedBox(
              width: 16,
              height: 16,
              child: CircularProgressIndicator(strokeWidth: 2))
          : child,
    );
  }
}

/// DegradedBanner 提示当前数字来自不可用的缓存。
///
/// 缺了它，空列表会被读成「没有流量」，而实际是「看不到流量」——
/// 这两件事在排查线上问题时会导向完全相反的结论。
class DegradedBanner extends StatelessWidget {
  const DegradedBanner({super.key, required this.message});

  final String message;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return Container(
      width: double.infinity,
      color: scheme.errorContainer,
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
      child: Row(
        children: [
          Icon(Icons.cloud_off, size: 16, color: scheme.onErrorContainer),
          const SizedBox(width: 8),
          Expanded(
            child: Text(message,
                style: TextStyle(color: scheme.onErrorContainer)),
          ),
        ],
      ),
    );
  }
}

/// outcomeColor 给 outcome 上色。context_exceeded 不算目标的失败
/// （输入太长是客户端的问题），所以它不用错误色。
Color outcomeColor(BuildContext context, String outcome) {
  final scheme = Theme.of(context).colorScheme;
  return switch (outcome) {
    'normal' => scheme.primary,
    'retrying' => scheme.tertiary,
    'context_exceeded' => scheme.outline,
    _ => scheme.error,
  };
}

class OutcomeChip extends StatelessWidget {
  const OutcomeChip({super.key, required this.outcome});

  final String outcome;

  @override
  Widget build(BuildContext context) {
    if (outcome.isEmpty) return const Text('-');
    final color = outcomeColor(context, outcome);
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
      decoration: BoxDecoration(
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(outcome, style: TextStyle(color: color, fontSize: 12)),
    );
  }
}

String stamp(DateTime? t) {
  if (t == null) return '-';
  return '${t.month.toString().padLeft(2, '0')}-${t.day.toString().padLeft(2, '0')} '
      '${t.hour.toString().padLeft(2, '0')}:${t.minute.toString().padLeft(2, '0')}:'
      '${t.second.toString().padLeft(2, '0')}';
}

/// compact 把 token 数缩成 k/M，表格里长数字会把列挤爆。
String compact(int n) {
  if (n >= 1000000) return '${(n / 1000000).toStringAsFixed(1)}M';
  if (n >= 1000) return '${(n / 1000).toStringAsFixed(1)}k';
  return '$n';
}
