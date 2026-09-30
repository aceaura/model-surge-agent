import 'package:flutter/material.dart';

import '../theme.dart';

/// 悬停自动亮起的列表行(参照 CC Switch,与姊妹仓同款):静止时白底 + 灰色
/// 圆角单线描边、无投影;鼠标进入后底色向 primarySoft 融合、描边转主色,
/// 130ms 过渡。仅外壳变化,内部布局不变。
class HoverCard extends StatefulWidget {
  final Widget child;
  const HoverCard({super.key, required this.child});

  @override
  State<HoverCard> createState() => _HoverCardState();
}

class _HoverCardState extends State<HoverCard> {
  bool _hover = false;

  @override
  Widget build(BuildContext context) {
    final t = context.tokens;
    final dark = Theme.of(context).brightness == Brightness.dark;
    // 与所在内容区底色一致的"静止色":亮模式纯白,暗模式 bg
    final base = dark ? t.bg : Colors.white;
    final bg = _hover
        ? Color.alphaBlend(t.primarySoft.withValues(alpha: .5), base)
        : base;
    final border = _hover ? t.primary.withValues(alpha: .45) : t.border;
    return MouseRegion(
      onEnter: (_) => setState(() => _hover = true),
      onExit: (_) => setState(() => _hover = false),
      child: AnimatedContainer(
        duration: const Duration(milliseconds: 130),
        curve: Curves.easeOut,
        decoration: BoxDecoration(
          color: bg,
          borderRadius: BorderRadius.circular(AppConst.radiusCard),
          border: Border.all(color: border),
        ),
        child: widget.child,
      ),
    );
  }
}
