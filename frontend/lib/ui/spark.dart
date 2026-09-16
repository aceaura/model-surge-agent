/// 分钟桶的柱状趋势图。
///
/// 自绘而非引入图表库：只需要「按分钟看有没有量、有没有突刺」，
/// 一个库换来的是配置面与体积。
library;

import 'package:flutter/material.dart';

class Spark extends StatelessWidget {
  const Spark({
    super.key,
    required this.values,
    required this.color,
    this.height = 72,
  });

  final List<int> values;
  final Color color;
  final double height;

  @override
  Widget build(BuildContext context) {
    if (values.isEmpty) {
      return SizedBox(
        height: height,
        child: Center(
          child: Text('窗口内没有数据',
              style: Theme.of(context).textTheme.bodySmall),
        ),
      );
    }
    return SizedBox(
      height: height,
      width: double.infinity,
      child: CustomPaint(painter: _SparkPainter(values, color)),
    );
  }
}

class _SparkPainter extends CustomPainter {
  _SparkPainter(this.values, this.color);

  final List<int> values;
  final Color color;

  @override
  void paint(Canvas canvas, Size size) {
    final peak = values.reduce((a, b) => a > b ? a : b);
    // 全零窗口按 1 归一，否则会除零并画满高。
    final scale = peak == 0 ? 1 : peak;
    final slot = size.width / values.length;
    final barWidth = (slot * 0.7).clamp(1.0, 14.0);
    final paint = Paint()..color = color;

    for (var i = 0; i < values.length; i++) {
      final ratio = values[i] / scale;
      // 有量的桶至少画 2px：1 次请求与 0 次请求在图上必须看得出差别。
      final h = values[i] == 0 ? 0.0 : (ratio * size.height).clamp(2.0, size.height);
      final left = i * slot + (slot - barWidth) / 2;
      canvas.drawRect(Rect.fromLTWH(left, size.height - h, barWidth, h), paint);
    }
  }

  @override
  bool shouldRepaint(_SparkPainter old) =>
      old.color != color || !_same(old.values, values);

  static bool _same(List<int> a, List<int> b) {
    if (a.length != b.length) return false;
    for (var i = 0; i < a.length; i++) {
      if (a[i] != b[i]) return false;
    }
    return true;
  }
}
