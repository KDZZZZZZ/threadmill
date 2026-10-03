set datafile separator comma
set datafile columnheaders
set encoding utf8
if (exists('PNG')) {
    set terminal pngcairo size 1500,1350 enhanced font 'DejaVu Sans,15' background rgb 'white'
    set output 'primary-runtime.png'
} else {
    set terminal svg size 1500,1350 enhanced font 'DejaVu Sans,15' background rgb 'white'
    set output 'primary-runtime.svg'
}
set border 3 lc rgb '#75818b' lw 1
set tics nomirror scale 0.5
set grid ytics lc rgb '#e2e6e9' lw 1
set style line 1 lc rgb '#505b66' lw 2.5 pt 5 ps 0.8
set style line 2 lc rgb '#1673aa' lw 2.5 pt 7 ps 0.8 dt 2
set style line 3 lc rgb '#bf6426' lw 2.5 pt 9 ps 0.9 dt 3
set xrange [40:610]
set xtics (64,128,192,256,384,448,500,576)
set key at screen 0.5, screen 0.952 center top horizontal maxrows 1 samplen 2 spacing 1 font ',12'
set multiplot layout 3,2 rowsfirst margins 0.095,0.985,0.13,0.895 spacing 0.105,0.075 title 'Runtime cost and physical storage | fixed trace, 3 completed repeats per tier*' font ',20'
set title 'Synthetic: 3000 files × 4 KiB'
set ylabel 'Wall (seconds)'
set yrange [0:500]
plot 'synthetic-pi-worktree.csv' using 1:2 with linespoints ls 1 title 'Pi + worktree', 'synthetic-threadmill-external.csv' using 1:2 with linespoints ls 2 title 'TM external', 'synthetic-threadmill-bwrap.csv' using 1:2 with linespoints ls 3 title 'TM bwrap'
set title 'IPython: 473 tracked files'
unset key
set label 1 '*' at 576,205 offset 1,0 tc rgb '#1673aa'
plot 'ipython-pi-worktree.csv' using 1:2 with linespoints ls 1, 'ipython-threadmill-external.csv' using 1:2 with linespoints ls 2, 'ipython-threadmill-bwrap.csv' using 1:2 with linespoints ls 3
unset label 1
unset title
set ylabel 'Observed peak increase (GB)'
set yrange [0:10]
plot 'synthetic-pi-worktree.csv' using 1:3 with linespoints ls 1, 'synthetic-threadmill-external.csv' using 1:3 with linespoints ls 2, 'synthetic-threadmill-bwrap.csv' using 1:3 with linespoints ls 3
plot 'ipython-pi-worktree.csv' using 1:3 with linespoints ls 1, 'ipython-threadmill-external.csv' using 1:3 with linespoints ls 2, 'ipython-threadmill-bwrap.csv' using 1:3 with linespoints ls 3
set xlabel 'Logical agents'
set ylabel 'Retained increase (MB)'
set yrange [0:2400]
plot 'synthetic-pi-worktree.csv' using 1:4 with linespoints ls 1, 'synthetic-threadmill-external.csv' using 1:4 with linespoints ls 2, 'synthetic-threadmill-bwrap.csv' using 1:4 with linespoints ls 3
set label 101 'GB/MB are decimal. Peak: sampled every 200 ms. Retained: after collect/release. Increases exclude fixture allocation.' at screen 0.095, screen 0.057 font ',12' left
set label 102 '* IPython / TM external / 576: 2 original + 1 supplemental result; an unresolved interrupted attempt remains. Supplement excluded from capacity.' at screen 0.095, screen 0.039 font ',12' left
set label 103 'Source: b839b28, primary-metrics.csv. Points are medians of completed repeats, not confidence intervals.' at screen 0.095, screen 0.021 font ',12' left
plot 'ipython-pi-worktree.csv' using 1:4 with linespoints ls 1, 'ipython-threadmill-external.csv' using 1:4 with linespoints ls 2, 'ipython-threadmill-bwrap.csv' using 1:4 with linespoints ls 3
unset multiplot
unset output
