set datafile separator comma
set datafile columnheaders
set encoding utf8
if (exists('PNG')) {
    set terminal pngcairo size 1500,650 enhanced font 'DejaVu Sans,15' background rgb 'white'
    set output 'write-latency.png'
} else {
    set terminal svg size 1500,650 enhanced font 'DejaVu Sans,15' background rgb 'white'
    set output 'write-latency.svg'
}
set border 3 lc rgb '#75818b' lw 1
set tics nomirror scale 0.5
set grid ytics lc rgb '#e2e6e9' lw 1
set style line 1 lc rgb '#505b66' lw 2.5 pt 5 ps 0.8
set style line 2 lc rgb '#1673aa' lw 2.5 pt 7 ps 0.8 dt 2
set style line 3 lc rgb '#bf6426' lw 2.5 pt 9 ps 0.9 dt 3
set xrange [40:610]
set xtics (64,128,192,256,384,448,500,576)
set yrange [0.1:30000]
set logscale y 10
set ytics (0.1,1,10,100,1000,10000)
set ylabel 'File-write P95 (ms, logarithmic axis)'
set xlabel 'Logical agents'
set key at screen 0.5, screen 0.90 center top horizontal maxrows 1 samplen 2 spacing 1 font ',12'
set multiplot layout 1,2 rowsfirst margins 0.095,0.985,0.20,0.78 spacing 0.105,0.075 title 'Write latency varies with the fixture | per-run P95, median across 3 completed repeats*' font ',20'
set title 'Synthetic: 3000 files × 4 KiB'
plot 'synthetic-pi-worktree.csv' using 1:5 with linespoints ls 1 title 'Pi + worktree', 'synthetic-threadmill-external.csv' using 1:5 with linespoints ls 2 title 'TM external', 'synthetic-threadmill-bwrap.csv' using 1:5 with linespoints ls 3 title 'TM bwrap'
unset key
set title 'IPython: 473 tracked files'
set label 101 '* IPython / TM external / 576 includes one supplemental result; the interrupted attempt remains unresolved.' at screen 0.095, screen 0.065 font ',12' left
set label 102 'Source: b839b28, primary-metrics.csv. Lines show medians, not confidence intervals. Values apply to this fixed trace and host.' at screen 0.095, screen 0.030 font ',12' left
plot 'ipython-pi-worktree.csv' using 1:5 with linespoints ls 1, 'ipython-threadmill-external.csv' using 1:5 with linespoints ls 2, 'ipython-threadmill-bwrap.csv' using 1:5 with linespoints ls 3
unset multiplot
unset output
